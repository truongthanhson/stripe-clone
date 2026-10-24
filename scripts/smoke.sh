#!/usr/bin/env bash
# End-to-end smoke test against a running API: every Phase 1 and 2 feature and hosted checkout, through real HTTP.
#
# Usage: SK=sk_test_... [BASE_URL=http://localhost:8080] scripts/smoke.sh
# Needs curl and jq. Creates real test-mode objects on the server it points at.
set -euo pipefail

BASE_URL=${BASE_URL:-http://localhost:8080}
: "${SK:?set SK to a test secret key (sk_test_...)}"
command -v jq >/dev/null || { echo "jq is required"; exit 1; }
RUN=$(date +%s)-$RANDOM # makes Idempotency-Keys unique per run

pass() { printf '  \033[32m✓\033[0m %s\n' "$1"; }
fail() { printf '  \033[31m✗\033[0m %s\n' "$1"; echo "    response: $BODY" >&2; exit 1; }

# api METHOD PATH [curl args...]: sets STATUS, BODY and HEADERS.
api() {
  local method=$1 path=$2; shift 2
  local headers_file; headers_file=$(mktemp)
  BODY=$(curl -sS -X "$method" "$BASE_URL$path" -H "Authorization: Bearer $SK" -D "$headers_file" -w '\n%{http_code}' "$@")
  STATUS=${BODY##*$'\n'}
  BODY=${BODY%$'\n'*}
  HEADERS=$(cat "$headers_file"); rm -f "$headers_file"
}
field() { jq -r "$1" <<<"$BODY"; }
expect() { # expect STATUS JQ_EXPR DESCRIPTION
  [[ $STATUS == "$1" ]] && [[ $(field "$2") == true ]] && pass "$3" || fail "$3 (HTTP $STATUS)"
}

echo "Smoke test against $BASE_URL"

echo "Basics"
BODY=$(curl -sS "$BASE_URL/healthz"); [[ $(field .status) == ok ]] && pass "healthz: API and database up" || fail healthz
api GET /v1/account; expect 200 '.object == "account" and .livemode == false' "test key authenticates"
grep -qi '^stripe-version:' <<<"$HEADERS" && pass "response carries Stripe-Version" || fail "Stripe-Version header"

echo "Customers"
api POST /v1/customers -d email="smoke-$RUN@example.com" -d name="Smoke Test" -d "metadata[run]=$RUN"
expect 200 '.id | startswith("cus_")' "create customer"; CUS=$(field .id)

echo "Payments"
api POST /v1/payment_intents -H "Idempotency-Key: charge-$RUN" -d amount=2000 -d currency=usd \
  -d payment_method=pm_card_visa -d confirm=true -d customer="$CUS" -d "metadata[order_id]=$RUN"
expect 200 '.status == "succeeded" and .amount_received == 2000' "charge a card in one call"; PI=$(field .id)

api POST /v1/payment_intents -H "Idempotency-Key: charge-$RUN" -d amount=2000 -d currency=usd \
  -d payment_method=pm_card_visa -d confirm=true -d customer="$CUS" -d "metadata[order_id]=$RUN"
expect 200 ".id == \"$PI\"" "retry with the same Idempotency-Key returns the same payment"
grep -qi '^idempotent-replayed: true' <<<"$HEADERS" && pass "retry is marked Idempotent-Replayed" || fail "Idempotent-Replayed header"

api POST /v1/payment_intents -H "Idempotency-Key: charge-$RUN" -d amount=9999 -d currency=usd
expect 400 '.error.type == "idempotency_error"' "same key with different parameters is rejected"

api POST /v1/payment_intents -d amount=2000 -d currency=usd -d payment_method=pm_card_chargeDeclinedInsufficientFunds -d confirm=true
expect 402 '.error.decline_code == "insufficient_funds" and .error.payment_intent.status == "requires_payment_method"' "declined card returns 402 card_error"
DECLINED=$(field .error.payment_intent.id)
api POST "/v1/payment_intents/$DECLINED/confirm" -d payment_method=pm_card_visa
expect 200 '.status == "succeeded"' "declined payment succeeds with another card"

api POST /v1/payment_intents -d amount=5000 -d currency=eur -d capture_method=manual -d payment_method=pm_card_visa -d confirm=true
expect 200 '.status == "requires_capture" and .amount_capturable == 5000' "authorize only (manual capture)"; AUTH=$(field .id)
api POST "/v1/payment_intents/$AUTH/capture" -d amount_to_capture=4000
expect 200 '.status == "succeeded" and .amount_received == 4000' "capture part of an authorization"

api POST /v1/payment_intents -d amount=3000 -d currency=usd -d capture_method=manual -d payment_method=pm_card_visa -d confirm=true
HOLD=$(field .id)
api POST "/v1/payment_intents/$HOLD/cancel" -d cancellation_reason=requested_by_customer
expect 200 '.status == "canceled"' "cancel releases an authorization"
api POST "/v1/payment_intents/$HOLD/capture"
expect 400 '.error.code == "payment_intent_unexpected_state"' "a canceled payment cannot be captured"

api POST /v1/payment_intents -d amount=1500 -d currency=usd -d payment_method=pm_card_timeout -d confirm=true
expect 200 '.status == "processing"' "network timeout leaves the payment processing (never guessed)"

api POST /v1/payment_intents -d amount=2000 -d currency=usd -d amout=1
expect 400 '.error.code == "parameter_unknown" and .error.param == "amout"' "unknown parameter is named in the error"

api GET "/v1/payment_intents?customer=$CUS&limit=10"
expect 200 "(.data | length) == 1 and .data[0].id == \"$PI\"" "list a customer's payments"
api GET "/v1/payment_intents?limit=2"
expect 200 '.object == "list" and (.data | length) == 2 and .has_more == true' "list pages with has_more"
api GET "/v1/payment_intents?limit=2&starting_after=$(field '.data[1].id')"
expect 200 '(.data | length) >= 1' "next page with starting_after"

echo "Refunds and balance"
api POST /v1/refunds -d payment_intent="$PI" -d amount=500 -d reason=requested_by_customer
expect 200 '.status == "succeeded" and .amount == 500' "partial refund"
api POST /v1/refunds -d payment_intent="$PI" -d amount=1501
expect 400 '.error.code == "amount_too_large"' "cannot refund more than remains"
api GET "/v1/refunds?payment_intent=$PI"
expect 200 '(.data | length) == 1' "list a payment's refunds"

api GET /v1/balance
expect 200 '.object == "balance" and ([.pending[] | select(.currency == "usd")] | length) == 1' "balance shows pending usd"
api GET "/v1/balance_transactions?limit=1"
expect 200 '.data[0].type == "refund" and .data[0].net == -500' "latest balance transaction is the refund"

echo "API versions"
api POST /v1/payment_intents -H "Stripe-Version: 2026-10-01" -d amount=2000 -d currency=usd
expect 200 '.capture_method == "automatic"' "an old version gets the old default"
api POST /v1/payment_intents -d amount=2000 -d currency=usd
expect 200 '.capture_method == "automatic_async"' "the latest version gets the new default"

echo "Hosted checkout"
api POST /v1/checkout/sessions -d mode=payment -d "success_url=https://shop.example/thanks?cs={CHECKOUT_SESSION_ID}" \
  -d "line_items[0][price_data][currency]=usd" -d "line_items[0][price_data][unit_amount]=1500" \
  -d "line_items[0][price_data][product_data][name]=T-shirt" -d "line_items[0][quantity]=2"
expect 200 '.status == "open" and .amount_total == 3000 and (.url | contains("/c/pay/cs_test_"))' "create a checkout session"; CS=$(field .id)
PAGE=$(curl -sS -o /dev/null -w '%{http_code}' "$BASE_URL/c/pay/$CS")
[[ $PAGE == 200 ]] && pass "payment page loads" || { BODY=$PAGE; fail "payment page"; }
LOCATION=$(curl -sS -o /dev/null -w '%{redirect_url}' "$BASE_URL/c/pay/$CS" \
  -d email=buyer@example.com -d number="4242 4242 4242 4242" -d exp="12 / 34" -d cvc=123)
[[ $LOCATION == "https://shop.example/thanks?cs=$CS" ]] && pass "pay on the page, redirected to success_url" || { BODY=$LOCATION; fail "checkout redirect"; }
api GET "/v1/checkout/sessions/$CS"
expect 200 '.status == "complete" and .payment_status == "paid" and .customer_details.email == "buyer@example.com"' "session is complete and paid"

echo "Customers, again"
api DELETE "/v1/customers/$CUS"
expect 200 '.deleted == true' "delete a customer"
api GET "/v1/customers/$CUS"
expect 200 '.deleted == true' "a deleted customer reads as deleted"

echo "All checks passed."
