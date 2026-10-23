package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/truongthanhson/stripe-clone/internal/ledger"
)

// TestE2EPhase2Gate is the Phase 2 gate: thousands of random, concurrent payment operations, after
// which every ledger transaction sums to zero, every cached balance matches its entries, and the
// ledger agrees both with the card network and with the payment objects.
//
// The seed is logged; rerun a failure with GATE_SEED=<seed>. GATE_OPS sets the number of operations.
func TestE2EPhase2Gate(t *testing.T) {
	seed := uint64(time.Now().UnixNano())
	if s, err := strconv.ParseUint(os.Getenv("GATE_SEED"), 10, 64); err == nil {
		seed = s
	}
	ops := 2000
	if n, err := strconv.Atoi(os.Getenv("GATE_OPS")); err == nil {
		ops = n
	}
	t.Logf("GATE_SEED=%d GATE_OPS=%d", seed, ops)

	e := newE2E(t)
	keys := []string{e.testKey, e.otherMerchantKey}
	currencies := []struct {
		code string
		min  int
	}{{"usd", 50}, {"eur", 50}, {"gbp", 30}, {"jpy", 50}, {"vnd", 10_000}}
	cards := []string{"pm_card_visa", "pm_card_visa", "pm_card_visa", "pm_card_mastercard",
		"pm_card_chargeDeclined", "pm_card_chargeDeclinedInsufficientFunds", "pm_card_refundFail"}

	// Shared pools of objects the workers act on. Racing on the same object is the point:
	// two workers may capture, cancel or refund the same PaymentIntent at once.
	type owned struct{ key, id string }
	var mu sync.Mutex
	var authorized, succeeded []owned
	pick := func(r *rand.Rand, pool *[]owned) (owned, bool) {
		mu.Lock()
		defer mu.Unlock()
		if len(*pool) == 0 {
			return owned{}, false
		}
		return (*pool)[r.IntN(len(*pool))], true
	}
	add := func(pool *[]owned, o owned) {
		mu.Lock()
		*pool = append(*pool, o)
		mu.Unlock()
	}

	// Status codes the API may legitimately answer; anything else (a 500) fails the gate.
	allowed := map[int]bool{200: true, 400: true, 402: true}
	var unexpected []string
	check := func(op string, code int, body []byte) {
		if !allowed[code] {
			mu.Lock()
			unexpected = append(unexpected, fmt.Sprintf("%s -> %d %s", op, code, body))
			mu.Unlock()
		}
	}

	const workers = 8
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewPCG(seed, uint64(w)))
			for range ops / workers {
				key := keys[r.IntN(len(keys))]
				cur := currencies[r.IntN(len(currencies))]
				amount := strconv.Itoa(cur.min + r.IntN(200_000))
				card := cards[r.IntN(len(cards))]

				switch op := r.IntN(10); {
				case op < 4: // charge
					rec := e.post(t, "/v1/payment_intents", key, url.Values{"amount": {amount}, "currency": {cur.code},
						"payment_method": {card}, "confirm": {"true"}}, nil)
					check("charge", rec.Code, rec.Body.Bytes())
					if rec.Code == 200 {
						add(&succeeded, owned{key, decode(t, rec.Body.Bytes())["id"].(string)})
					}
				case op < 6: // authorize only
					rec := e.post(t, "/v1/payment_intents", key, url.Values{"amount": {amount}, "currency": {cur.code},
						"payment_method": {card}, "confirm": {"true"}, "capture_method": {"manual"}}, nil)
					check("authorize", rec.Code, rec.Body.Bytes())
					if rec.Code == 200 {
						add(&authorized, owned{key, decode(t, rec.Body.Bytes())["id"].(string)})
					}
				case op < 7: // capture, often partially
					if o, ok := pick(r, &authorized); ok {
						form := url.Values{}
						if r.IntN(2) == 0 {
							// Around the currency minimum, so too-small captures are exercised too.
							form.Set("amount_to_capture", strconv.Itoa(cur.min/2+r.IntN(cur.min*20)))
						}
						rec := e.post(t, "/v1/payment_intents/"+o.id+"/capture", o.key, form, nil)
						check("capture", rec.Code, rec.Body.Bytes())
						if rec.Code == 200 {
							add(&succeeded, o)
						}
					}
				case op < 8: // cancel
					if o, ok := pick(r, &authorized); ok {
						rec := e.post(t, "/v1/payment_intents/"+o.id+"/cancel", o.key, url.Values{}, nil)
						check("cancel", rec.Code, rec.Body.Bytes())
					}
				default: // refund, partial or full
					if o, ok := pick(r, &succeeded); ok {
						form := url.Values{"payment_intent": {o.id}}
						if r.IntN(3) > 0 {
							form.Set("amount", strconv.Itoa(1+r.IntN(5_000)))
						}
						rec := e.post(t, "/v1/refunds", o.key, form, nil)
						check("refund", rec.Code, rec.Body.Bytes())
					}
				}
			}
		}()
	}
	wg.Wait()

	if len(unexpected) > 0 {
		t.Fatalf("%d unexpected responses, first: %s", len(unexpected), unexpected[0])
	}
	ctx := context.Background()

	t.Run("every transaction balances and every cached balance matches its entries", func(t *testing.T) {
		e.checkLedger(t)
	})

	t.Run("the ledger agrees with the card network", func(t *testing.T) {
		s := e.network.Stats()
		var receivable int64
		_ = e.pool.QueryRow(ctx, `SELECT COALESCE(SUM(balance), 0) FROM ledger_accounts WHERE type = 'asset'`).Scan(&receivable)
		// Amounts in different currencies are summed here only because the fake network does the same.
		if receivable != s.Captured-s.Refunded {
			t.Fatalf("ledger receivable %d, network captured %d - refunded %d", receivable, s.Captured, s.Refunded)
		}
	})

	t.Run("one posting per succeeded payment and refund, none for anything else", func(t *testing.T) {
		checks := map[string]string{
			"succeeded payment without a charge": `SELECT count(*) FROM payment_intents p WHERE status = 'succeeded'
				AND NOT EXISTS (SELECT 1 FROM ledger_transactions t WHERE t.type = 'charge' AND t.source_id = p.id)`,
			"charge for a payment that did not succeed": `SELECT count(*) FROM ledger_transactions t JOIN payment_intents p
				ON p.id = t.source_id WHERE t.type = 'charge' AND p.status <> 'succeeded'`,
			"succeeded refund without a posting": `SELECT count(*) FROM refunds r WHERE status = 'succeeded'
				AND NOT EXISTS (SELECT 1 FROM ledger_transactions t WHERE t.type = 'refund' AND t.source_id = r.id)`,
			"posting for a refund that did not succeed": `SELECT count(*) FROM ledger_transactions t JOIN refunds r
				ON r.id = t.source_id WHERE t.type = 'refund' AND r.status <> 'succeeded'`,
			"payment refunded beyond what it received": `SELECT count(*) FROM payment_intents p WHERE amount_received <
				(SELECT COALESCE(SUM(amount), 0) FROM refunds r WHERE r.payment_intent_id = p.id AND r.status = 'succeeded')`,
		}
		for name, query := range checks {
			if n := e.count(t, query); n != 0 {
				t.Errorf("%s: %d", name, n)
			}
		}
	})

	t.Run("each merchant balance equals its payments minus fees minus refunds", func(t *testing.T) {
		rows, err := e.pool.Query(ctx, `SELECT merchant_id, currency, amount_received FROM payment_intents WHERE status = 'succeeded'`)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]int64{}
		for rows.Next() {
			var merchant int64
			var currency string
			var received int64
			_ = rows.Scan(&merchant, &currency, &received)
			want[ledger.MerchantPending(merchant, false, currency).Code] += received - ledger.Fee(currency, received)
		}
		rows, err = e.pool.Query(ctx, `SELECT merchant_id, currency, amount FROM refunds WHERE status = 'succeeded'`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var merchant int64
			var currency string
			var amount int64
			_ = rows.Scan(&merchant, &currency, &amount)
			want[ledger.MerchantPending(merchant, false, currency).Code] -= amount
		}

		rows, err = e.pool.Query(ctx, `SELECT code, -balance FROM ledger_accounts WHERE merchant_id IS NOT NULL`)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]int64{}
		for rows.Next() {
			var code string
			var balance int64
			_ = rows.Scan(&code, &balance)
			got[code] = balance
		}
		if len(want) == 0 {
			t.Fatal("no money moved; the gate tested nothing")
		}
		for code, w := range want {
			if got[code] != w {
				t.Errorf("%s: ledger %d, payments say %d", code, got[code], w)
			}
		}
		if len(got) != len(want) {
			t.Errorf("ledger has %d merchant accounts, payments imply %d", len(got), len(want))
		}
	})

	var txns, entries, charges, refunds int
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions`).Scan(&txns)
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries`).Scan(&entries)
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM payment_intents WHERE status = 'succeeded'`).Scan(&charges)
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM refunds WHERE status = 'succeeded'`).Scan(&refunds)
	t.Logf("%d operations: %d succeeded payments, %d succeeded refunds, %d ledger transactions, %d entries",
		ops, charges, refunds, txns, entries)
}
