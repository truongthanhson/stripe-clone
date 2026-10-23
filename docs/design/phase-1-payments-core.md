# Phase 1 design note: payments core

Phase 1 built the PaymentIntent API: create, retrieve, update, list, confirm, capture and cancel,
with idempotency, a card network boundary, an OpenAPI contract and dated versions.
**Gate:** 50 concurrent retries with one Idempotency-Key produce exactly one charge
(`TestE2EPhase1Gate` in `cmd/api/lifecycle_test.go`). It passes in CI.

## The state machine

```
requires_payment_method ──confirm──▶ processing ──approved, automatic──▶ succeeded
        ▲   │                           │  │
        │   └─(payment_method)─▶ requires_confirmation ──confirm──┘
        │                               │  └──approved, manual──▶ requires_capture ──capture──▶ succeeded
        └────────────declined───────────┘                                │
                                                                         └──cancel (void)──▶ canceled
any status before processing ──cancel──▶ canceled
```

The allowed moves live in one table (`internal/paymentintent/paymentintent.go`); a test fails if a
status is added without deciding its moves. `succeeded` and `canceled` are terminal.

## Decisions

| Decision | Why | Alternative rejected |
|---|---|---|
| Compare-and-swap status updates (`UPDATE ... WHERE status = $from`) | One of N concurrent requests wins without holding a lock | `SELECT ... FOR UPDATE` across the bank call: blocks the row and exhausts the pool when the bank is slow |
| Never hold a DB transaction across a network call | The bank can take seconds; transactions should take milliseconds | One big transaction: simpler, but it couples DB health to bank latency |
| Idempotency keys stored in Postgres, the primary key is the lock | Same database, same guarantees, no extra system | Redis: one more thing to run and to lose on restart |
| Network reference = `pi_id:attempt` | The network dedupes retries of one attempt; a new card is a new attempt | `pi_id` alone: a retry after a decline would get the old decline back |
| Form-encoded requests, Stripe error shape with `param` | Stripe users' habits and tools carry over | JSON bodies: more familiar, but every Stripe example would need translating |
| OpenAPI spec enforced both ways | Inputs rejected unless declared; responses must match | Spec as documentation only: drifts within weeks |
| Versions as response downgrades | Handlers only know the latest shape | `if version < X` in handlers: grows with every change |

## Failure cases and what happens

| Failure | Result | Proven by |
|---|---|---|
| Client retries a POST (timeout, double click) | Same response replayed; nothing runs twice | `TestE2EIdempotency`, `TestE2EPhase1Gate` |
| Two confirms race without a key | One charges, the other gets `payment_intent_unexpected_state` | `TestE2EPhase1Gate` (no-key case) |
| Capture races cancel | Network serialises them; DB status always matches the network | `TestE2ECaptureRacesCancel` |
| Card declined | 402 `card_error`; PaymentIntent back to `requires_payment_method`, retry with another card | `TestE2ELifecycle` |
| Bank approves but the answer is lost | PaymentIntent stays `processing`; never guessed as failed | `TestE2ELifecycle` (pm_card_timeout) |
| Process dies mid-request | Idempotency key is taken over after 1 minute by the same request | `internal/idempotency` tests |
| Test key reads live data | 404, indistinguishable from "does not exist" | `TestE2EPaymentIntentScoping` |

## Known gaps, scheduled

- **`processing` after a timeout is not resolved yet.** Phase 3 adds a worker that asks the network
  again with the same reference and settles the PaymentIntent (`internal/paymentintent/service.go`
  logs these today).
- **Idempotency is two transactions** (handler, then stored response). A crash between them can make
  a create run twice (a second PaymentIntent, never a second charge). Phase 3's outbox work adds
  recovery points in the same transaction.
- **Live mode cannot confirm** until a real processor adapter (Stripe test mode) is plugged into
  `cardnetwork.Network`. It needs a Stripe account's test key.
- **Hosted checkout** (from the Phase 1 roadmap) moves to after the dashboard, since both need
  browser sessions and HTML templates.
