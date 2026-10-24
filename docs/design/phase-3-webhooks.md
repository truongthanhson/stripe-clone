# Phase 3 design note: events, webhooks and unknown outcomes

Phase 3 tells merchants what happened (events and webhooks) and makes the system recover by itself
when the bank does not answer. Two gates prove it:

- **Webhooks:** a receiver that fails a third of the time (500s, dropped connections, answers lost
  after it processed the event) still processes every event exactly once by id, in the order the
  events happened, with every signature valid (`TestE2EWebhooksGate` in `cmd/api/webhooks_test.go`).
- **Recovery:** with 40% of the messages to and from the bank lost, concurrent charges, captures,
  cancels and refunds all settle, and our books match the bank to the cent: every approved hold is
  captured, voided or still held exactly as its payment says, every refund the bank paid is recorded
  once, the ledger balances, and each outcome is announced by exactly one event
  (`TestE2EPhase3Gate` in `cmd/api/recovery_test.go`; `GATE_SEED` replays, `GATE_OPS` scales).

Both pass.

## Events: a transactional outbox

```
 request ──► BEGIN
              UPDATE payment_intents ... WHERE status = 'processing'   (compare-and-swap)
              INSERT ledger entries                                    (if money moved)
              UPDATE checkout_sessions ... WHERE status = 'open'       (if it ends a session)
              pg_advisory_xact_lock(merchant)                          (always last)
              INSERT events; INSERT webhook_deliveries (one per subscribed endpoint)
             COMMIT
```

The services (`paymentintent`, `refund`, `checkout`) expose hooks that run inside the transaction of
each change; `buildApp` in `cmd/api/main.go` connects them to `events.Record`. The payments code never
imports the events code.

| Decision | Why | Alternative rejected |
|---|---|---|
| Write the event in the change's transaction | An event exists exactly when its change committed: no webhook for a rolled-back payment, no committed payment without one | Publish after commit: a crash in between loses the event. Two-phase commit with a broker: heavy, and still needs an outbox to be safe |
| Deliveries created at record time, one row per endpoint | Fan-out is part of the same atomic write; each row carries its own retry state | Fan out later from the events table: another worker and another place to lose track |
| Per-merchant advisory lock, taken last | `seq` order is commit order, so an endpoint never sees event 6 before event 5 exists; taking it last means it cannot form a deadlock cycle with row locks | No lock: identity values are assigned at insert, not commit, so a reader can see a gap that fills later |
| Events stored in the merchant's pinned API version | A webhook payload has one shape; `api_version` on the event says which | Render per endpoint at delivery: endpoints would disagree about the same event |
| Checkout sessions settle in the PaymentIntent's transaction | `checkout.session.completed` fires even if nobody looks at the session afterwards (for example when the resolver finishes the payment) | Lazy settle on read only (Phase 2 hosted checkout): the event would wait for a read |

## Webhook delivery

At least once, in order per endpoint, retried for 3 days.

- **Claim.** One statement picks each endpoint's oldest pending delivery (`DISTINCT ON (endpoint_id)`)
  and claims it if it is due and not locked, setting `locked_until = now + 1 minute`. Two dispatchers
  racing on the same row: Postgres re-checks the `WHERE` on the row the winner changed, and the loser
  gets nothing. Only the oldest is eligible, so an endpoint has at most one delivery in flight and
  receives events in order; a later event never overtakes one being retried.
- **Send.** `POST` JSON with `Stripe-Signature: t=<unix>,v1=<hex HMAC-SHA256("t.body")>`. Any 2xx
  acknowledges it; anything else, a timeout or a redirect is a failure.
- **Record.** Success marks it succeeded. Failure schedules the next attempt after 1m, 5m, 30m, 2h, 5h,
  10h, then every 12h; once the next attempt would fall after 72 hours from the event, it is failed.
  The result is written with `context.WithoutCancel`, so shutting down mid-send still records it.
  If the process dies mid-send, the claim expires and another dispatcher sends it again.
- **Receivers** must deduplicate on the event id: a success whose answer is lost is sent again. The
  gate test's receiver does exactly that and processes each event once.

Security: signatures carry the timestamp inside the signed bytes, so a captured delivery cannot be
replayed after the receiver's tolerance (5 minutes in `webhook.Verify`). Comparison is constant time.
Outgoing connections refuse loopback, private, link-local and CGNAT addresses, checked in the dialer's
`Control` hook on the address actually dialed (after DNS), with no proxy and no redirects followed, so
a webhook URL cannot be used to reach our own network. `WEBHOOKS_ALLOW_PRIVATE_NETWORKS=true` lifts
that for local development. Live endpoints must use https. Secrets are returned only on create.

## Unknown outcomes

When the bank does not answer, we do not know whether money moved. Phase 1 already refused to guess
(a timed-out authorization stayed `processing`); Phase 3 makes the unknown explicit and resolves it.

| Call that timed out | What we record | What the merchant sees |
|---|---|---|
| Authorize (confirm) | status `processing`, `unresolved_op = authorize` | 200 with `status: processing` |
| Capture after an automatic authorization | `processing`, `unresolved_op = capture`, the authorization ID | 200 with `status: processing` |
| Capture of a manual authorization | `requires_capture`, `unresolved_op = capture`, the amount | 502 `card_network_unavailable`; a different capture or a cancel gets 409 `payment_intent_outcome_pending` |
| Void (cancel) | `requires_capture`, `unresolved_op = void` | 502; a capture gets 409 |
| Refund | refund `pending`, `unresolved_since` set; the amount stays reserved | 200 with `status: pending` |

The **resolver** (`resolver` in `cmd/api/resolver.go`, every 5 seconds) repeats the exact call. Every
network operation is idempotent: authorize on the attempt's reference, capture and void on the
authorization, refund on the refund's ID. So a repeat returns the original outcome if the first call
reached the bank, and performs it if it did not; either way the answer is definite once it arrives,
and it is recorded through the same compare-and-swap transitions (with ledger posting and events) as
a normal answer. A payment left `processing` or a refund left `pending` without a mark, which happens
only if the process died mid-call, is picked up after a grace period of twice the network timeout.

| Decision | Why | Alternative rejected |
|---|---|---|
| Repeat the same idempotent call | Works whether the request or the answer was lost; never creates a second hold or payout | Inquiry-only ("did you see this?"): if the request is still in flight it can arrive after "no", leaving an orphaned hold |
| Mark the unknown state in the row | The dashboard can say why a payment is stuck, the resolver finds work by index, and conflicting actions (capture after a lost void) are refused | Infer it from "processing for a while": slower, and cannot tell a lost capture of a manual payment from an untouched one |
| Only the same operation may be repeated while unresolved | A capture after a void that may have happened (or the reverse) would leave the bank and the books disagreeing | Let any action through: the bank could hold a capture we recorded as canceled |
| Resolver behind the same lossy link in the test | Proves recovery converges even when recovery itself loses messages | A perfect network for the resolver: hides retry bugs in the resolver |

## Failure cases and what happens

| Failure | Result | Proven by |
|---|---|---|
| Bank approves, answer lost (`pm_card_timeout`) | `processing` + unresolved; the resolver gets the original approval and finishes the charge, one hold | `TestE2EUnknownOutcomes` |
| Capture answer lost | 502; other actions get 409; the resolver repeats it and records the original amount | `TestE2EUnknownOutcomes` |
| Void answer lost | 502; the resolver repeats the void and cancels | `TestE2EUnknownOutcomes` |
| Refund answer lost | Refund pending, amount reserved; the resolver records it once, one ledger posting | `TestE2EUnknownOutcomes` |
| 40% of bank messages lost under concurrency | Everything settles; books match the bank; one event per outcome | `TestE2EPhase3Gate` |
| Receiver fails, drops connections, loses its own answers | Every event processed once by id, in order | `TestE2EWebhooksGate` |
| Receiver down for 3 days | 11 attempts, then the delivery is failed | `TestE2EWebhookRetriesEnd` |
| Webhook URL points at 127.0.0.1 | Refused before connecting | `TestE2EWebhooksRefusePrivateAddresses` |
| A hook (event write) fails | The payment change rolls back with it | `TestHookFailureRollsBackTheChange` |

## Not done yet

- Events and deliveries are kept forever; a pruning job (Stripe keeps events 30 days) comes with payouts.
- One slow endpoint holds up only itself, but each dispatcher pass waits for its slowest send (10s
  timeout). A per-endpoint worker pool would remove that coupling if endpoint counts grow.
- Customer events (`customer.created` and so on) are not recorded yet.
