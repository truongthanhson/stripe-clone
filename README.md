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

A declined card returns `402` with `{"error": {"type": "card_error", "code": "card_declined", "decline_code": ..., "payment_intent": {...}}}`;
the PaymentIntent goes back to `requires_payment_method` and can be confirmed again with another card.

Every POST accepts an `Idempotency-Key` header. A retry with the same key and parameters within 24 hours
returns the first response (with `Idempotent-Replayed: true`) instead of doing the work twice; the same key
with different parameters is rejected.

### Test cards

In test mode, pass one of these as `payment_method`. The built-in fake card network decides the outcome
from the card behind it.

| payment_method | Outcome |
|---|---|
| `pm_card_visa`, `pm_card_mastercard` | Approved |
| `pm_card_chargeDeclined` | Declined, `generic_decline` |
| `pm_card_chargeDeclinedInsufficientFunds` | Declined, `insufficient_funds` |
| `pm_card_chargeDeclinedLostCard` | Declined, `lost_card` |
| `pm_card_chargeDeclinedExpiredCard` | Declined, `expired_card` |
| `pm_card_chargeDeclinedIncorrectCvc` | Declined, `incorrect_cvc` |
| `pm_card_chargeDeclinedProcessingError` | Declined, `processing_error` |
| `pm_card_timeout` | The network approves but its answer never arrives (outcome unknown) |

## Deploy

See [docs/deploy.md](docs/deploy.md): `compose.prod.yaml` runs Postgres, migrations, the API and Caddy (HTTPS) on one VM.

## Test

```sh
go test ./...   # the migrations test starts its own Postgres via testcontainers, so Docker must be running
```
