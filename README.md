# stripe-clone

A small payments API built to learn backend and system design.

## Run

```sh
docker compose up -d --wait                                            # Postgres 17 on :5433
export DATABASE_URL='postgres://stripe:stripe@localhost:5433/stripe?sslmode=disable'
go run ./cmd/migrate up                                                # also: down, status
go run ./cmd/api                                                       # listens on :8080 (override with PORT)
curl localhost:8080/healthz                                            # 200 when the DB is up, 503 when down
```

## API keys

```sh
go run ./cmd/admin create-merchant -name Acme -email ops@acme.test    # prints the merchant id
go run ./cmd/admin issue-key -merchant 1 -mode test                    # prints sk_test_... once; only its hash is stored
curl -H "Authorization: Bearer sk_test_..." localhost:8080/v1/account
```

## Payments

Requests use form encoding, like Stripe (`curl -d` sends it). Amounts are integers in the currency's
smallest unit: `2000` with `usd` is $20.00.

```sh
curl localhost:8080/v1/payment_intents -H "Authorization: Bearer sk_test_..." \
  -d amount=2000 -d currency=usd -d description="Order #1"         # returns {"id": "pi_...", "status": "requires_payment_method", ...}
curl localhost:8080/v1/payment_intents/pi_... -H "Authorization: Bearer sk_test_..."
```

Charge a card in one call, or authorize now and capture later:

```sh
curl localhost:8080/v1/payment_intents -H "Authorization: Bearer sk_test_..." \
  -d amount=2000 -d currency=usd -d payment_method=pm_card_visa -d confirm=true      # status: succeeded

curl localhost:8080/v1/payment_intents -H "Authorization: Bearer sk_test_..." \
  -d amount=2000 -d currency=usd -d capture_method=manual                            # status: requires_payment_method
curl localhost:8080/v1/payment_intents/pi_.../confirm -H "Authorization: Bearer sk_test_..." \
  -d payment_method=pm_card_visa                                                      # status: requires_capture
curl localhost:8080/v1/payment_intents/pi_.../capture -H "Authorization: Bearer sk_test_..." \
  -d amount_to_capture=1500                                                           # status: succeeded (rest released)
curl -X POST localhost:8080/v1/payment_intents/pi_.../cancel -H "Authorization: Bearer sk_test_..."   # before capture: releases the hold
```

List newest first and page with the last ID you saw; attach your own key-value `metadata`:

```sh
curl "localhost:8080/v1/payment_intents?limit=10" -H "Authorization: Bearer sk_test_..."               # {"object":"list","has_more":true,"data":[...]}
curl "localhost:8080/v1/payment_intents?limit=10&starting_after=pi_..." -H "Authorization: Bearer sk_test_..."
curl localhost:8080/v1/payment_intents/pi_... -H "Authorization: Bearer sk_test_..." \
  -d "metadata[order_id]=6735" -d "metadata[old_key]="                               # sets order_id, removes old_key
```

Every captured charge is recorded in a double-entry ledger; the balance and its history are read from it:

```sh
curl localhost:8080/v1/balance -H "Authorization: Bearer sk_test_..."                 # pending: [{"amount": 1912, "currency": "usd"}]
curl localhost:8080/v1/balance_transactions -H "Authorization: Bearer sk_test_..."    # one per charge: amount, fee, net, source
```

Customers group payments by who paid:

```sh
curl localhost:8080/v1/customers -H "Authorization: Bearer sk_test_..." -d email=jenny@example.com -d name="Jenny Rosen"
curl localhost:8080/v1/payment_intents -H "Authorization: Bearer sk_test_..." -d amount=2000 -d currency=usd -d customer=cus_...
curl "localhost:8080/v1/payment_intents?customer=cus_..." -H "Authorization: Bearer sk_test_..."
curl -X DELETE localhost:8080/v1/customers/cus_... -H "Authorization: Bearer sk_test_..."   # {"id": "cus_...", "deleted": true}
```

Refund all or part of a succeeded payment; partial refunds can never add up to more than was received:

```sh
curl localhost:8080/v1/refunds -H "Authorization: Bearer sk_test_..." -d payment_intent=pi_... -d amount=500
curl "localhost:8080/v1/refunds?payment_intent=pi_..." -H "Authorization: Bearer sk_test_..."
```

A declined card returns `402` with `{"error": {"type": "card_error", "code": "card_declined", "decline_code": ..., "payment_intent": {...}}}`;
the PaymentIntent goes back to `requires_payment_method` and can be confirmed again with another card.

Every POST accepts an `Idempotency-Key` header. A retry with the same key and parameters within 24 hours
returns the first response (with `Idempotent-Replayed: true`) instead of doing the work twice; the same key
with different parameters is rejected.

### Test cards

In test mode, pass one of these as `payment_method`. The built-in fake card network decides the outcome
from the card behind it.

| payment_method | Card number (checkout page) | Outcome |
|---|---|---|
| `pm_card_visa`, `pm_card_mastercard` | `4242 4242 4242 4242`, `5555 5555 5555 4444` | Approved |
| `pm_card_chargeDeclined` | `4000 0000 0000 0002` | Declined, `generic_decline` |
| `pm_card_chargeDeclinedInsufficientFunds` | `4000 0000 0000 9995` | Declined, `insufficient_funds` |
| `pm_card_chargeDeclinedLostCard` | `4000 0000 0000 9987` | Declined, `lost_card` |
| `pm_card_chargeDeclinedExpiredCard` | `4000 0000 0000 0069` | Declined, `expired_card` |
| `pm_card_chargeDeclinedIncorrectCvc` | `4000 0000 0000 0127` | Declined, `incorrect_cvc` |
| `pm_card_chargeDeclinedProcessingError` | `4000 0000 0000 0119` | Declined, `processing_error` |
| `pm_card_timeout` | `4000 0000 0000 9003` | The network approves but its answer never arrives (outcome unknown) |
| `pm_card_refundFail` | `4000 0000 0000 5126` | Charges normally; every refund is refused by the issuer |

## Hosted checkout

Let a page on this server collect the card: create a Checkout Session and send the buyer to its `url`.
After paying they land on `success_url`, with `{CHECKOUT_SESSION_ID}` filled in.

```sh
curl localhost:8080/v1/checkout/sessions -H "Authorization: Bearer sk_test_..." \
  -d mode=payment -d "success_url=https://shop.example/thanks?session={CHECKOUT_SESSION_ID}" \
  -d cancel_url=https://shop.example/cart \
  -d "line_items[0][price_data][currency]=usd" -d "line_items[0][price_data][unit_amount]=1500" \
  -d "line_items[0][price_data][product_data][name]=T-shirt" -d "line_items[0][quantity]=2"
# {"id": "cs_test_...", "url": "http://localhost:8080/c/pay/cs_test_...", "status": "open", ...}
curl localhost:8080/v1/checkout/sessions/cs_test_... -H "Authorization: Bearer sk_test_..."   # status: complete once paid
curl -X POST localhost:8080/v1/checkout/sessions/cs_test_.../expire -H "Authorization: Bearer sk_test_..."
```

Open the `url` in a browser and pay with a test card number from the table below (any future expiry,
any CVC). Links use `PUBLIC_URL` (default `http://localhost:$PORT`); set it to the address browsers
reach the server on. A session expires after 24 hours (`expires_at` can shorten that).

## Dashboard

Merchants sign in at `/dashboard` with their email and a password. Set the password from the command line
(it is read from stdin, so it stays out of shell history):

```sh
read -s PW && echo "$PW" | go run ./cmd/admin set-password -merchant 1
open http://localhost:8080/dashboard
```

It shows payments (capture, cancel and refund them), customers, the balance with its transactions,
and API keys (create and revoke). Test and live data are kept apart; the switch in the header flips
between them. Five wrong passwords lock sign-in for 15 minutes. Pages are server-rendered with
`html/template`; htmx (served from `/static`) makes navigation and "Load more" swap in place,
and every page still works with JavaScript off.

## API versions

Each merchant is pinned to the API version that was current when they were created, so later breaking
changes never reach an existing integration. Send `Stripe-Version: 2026-10-01` to use a specific version
for one request; every response says which version rendered it. Re-pin a merchant after they have tested:

```sh
go run ./cmd/admin set-api-version -merchant 1 -version 2026-10-23
```

Changes are listed in `internal/apiversion` and in the spec's description.

## API reference

[`openapi/openapi.yaml`](openapi/openapi.yaml) is the source of truth, also served at `GET /openapi.yaml`.
The server rejects any parameter the spec does not declare, and the tests fail if a route or response
field exists in code but not in the spec (or the other way round). Change the spec first.

## Deploy

See [docs/deploy.md](docs/deploy.md): `compose.prod.yaml` runs Postgres, migrations, the API and Caddy (HTTPS) on one VM.

## Test

```sh
go test ./...   # the migrations test starts its own Postgres via testcontainers, so Docker must be running
```

The gate tests are the ones that prove the big guarantees: `TestE2EPhase1Gate` (50 concurrent retries
of one payment charge the card once) and `TestE2EPhase2Gate` (thousands of random charges, captures
and refunds keep the ledger balanced; `GATE_SEED` and `GATE_OPS` replay or scale a run).

### Smoke test a running server

`scripts/smoke.sh` walks every feature over real HTTP with `curl` and `jq`: charges, declines,
idempotent retries, auth and capture, cancel, timeouts, pagination, refunds, balance, API versions,
customers and paying a hosted checkout page. It creates its own data, so it can run again on the same database.

```sh
go run ./cmd/admin create-merchant -name Smoke -email smoke@test.local
SK=$(go run ./cmd/admin issue-key -merchant 1 -mode test | head -1)
go run ./cmd/api &
SK=$SK scripts/smoke.sh                       # BASE_URL=https://... scripts/smoke.sh for a deployed server
```

It exits non-zero on the first broken check and prints the response that broke it.
