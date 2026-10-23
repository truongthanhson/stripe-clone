# Phase 2 design note: ledger, refunds, customers

Phase 2 added a double-entry ledger under every movement of money, the balance API on top of it,
refunds and customers. Authorize-then-capture already existed from Phase 1 and now posts to the ledger.
**Gate:** after thousands of randomized, concurrent operations, every ledger transaction sums to zero
and every stored balance matches its entries (`TestE2EPhase2Gate` in `cmd/api/ledger_gate_test.go`,
2,000 operations in CI; run more with `GATE_OPS=20000`, replay a failure with `GATE_SEED=...`). It passes.

## The money model

Positive amounts are debits, negative are credits, every transaction sums to zero.

| Event | Debit | Credit |
|---|---|---|
| Charge captured (amount A, fee F) | network receivable A | merchant pending A−F, platform fees F |
| Refund R | merchant pending R | network receivable R |

Accounts exist per merchant, mode and currency (`merchant:7:test:usd:pending`), so test and live money
and different currencies can never mix. Postgres enforces the rules even if the Go code is wrong:
a deferred trigger rejects an unbalanced or mixed-currency transaction at COMMIT, and triggers refuse
any UPDATE or DELETE of history.

## Decisions

| Decision | Why | Alternative rejected |
|---|---|---|
| Post in the same DB transaction as the status change | "Succeeded" and "money recorded" can never disagree | Post afterwards or from a job: a crash in between leaves a succeeded payment missing from the books |
| Cached balance on each account, proven by `ledger.Check` | Reading a balance is one row; the check catches drift | SUM over all entries on every read: correct, but cost grows with history |
| `UNIQUE (type, source_id)` on ledger transactions | Posting the same event twice is a no-op | Rely on callers: one retry bug doubles money |
| Accounts locked in code order | Two postings on the same accounts can't deadlock | Lock in entry order: deadlocks under concurrency |
| Refunds reserve under a row lock on the PaymentIntent | The rule is a sum across rows, which a compare-and-swap can't guard | Check-then-insert without a lock: concurrent partial refunds exceed the charge |
| Fee is not returned on refund | Stripe's policy; keeps refund postings two-legged | Return the fee: possible later as a separate posting |
| Customers are soft-deleted | Payments keep a valid reference; matches Stripe's `deleted: true` | Hard delete: breaks payment history |

## Failure cases and what happens

| Failure | Result | Proven by |
|---|---|---|
| 20 concurrent partial refunds on one payment | Never more than received; network refunds exactly the sum that succeeded | `TestE2ERefunds` |
| Issuer refuses a refund | Refund `failed`, no money moved, amount refundable again | `TestE2ERefunds` (pm_card_refundFail) |
| Refund network call times out | Refund stays `pending` with the amount reserved | design; resolver in Phase 3 |
| A bug writes unbalanced entries | COMMIT fails; nothing is stored | `internal/ledger` store tests |
| Someone edits history by hand | UPDATE/DELETE refused | `internal/ledger` store tests |
| Cached balance drifts | `ledger.Check` reports the account | `internal/ledger` store tests |
| Capture of an amount the fee would swallow | Rejected as `amount_too_small` before reaching the network | `TestE2ELifecycle`; found by the gate test |

## What the gate test found

The first run of the gate failed: capturing 1 cent of an authorization made the fee equal to the
amount, the merchant's share was 0, and the ledger rightly refused a zero entry. But by then the
card network had already captured, so the money moved and the database rolled back. Two fixes:
captures now require the currency's minimum amount (as new charges do, and as Stripe does), and the
charge posting omits zero legs instead of failing. The deeper lesson stays open (next section).

## Known gaps, scheduled

- **"Network did it, database didn't."** If the commit after a successful network capture or refund
  fails (database down at that instant), the network has moved money our records don't show. It is
  logged as an error (`captured on the network but not recorded`); retrying the same capture is safe
  because the network capture is idempotent. Phase 3's resolver compares stuck objects with the
  network and settles them, and Phase 4's reconciliation catches anything left.
- **Hot platform accounts.** Every capture in a currency briefly locks `network_receivable` and `fees`.
  Fine at this scale; the fix is sharded sub-accounts or computing platform balances lazily.
- **Pending only.** Charges land in `pending`; moving funds to `available` and paying them out is Phase 4.
- **Merchant dashboard** (listed under Phase 2 in the roadmap) is not built yet; it needs browser
  sessions and templates, and is planned together with hosted checkout.
