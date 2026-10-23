package main

import (
	"net/url"
	"testing"

	"github.com/truongthanhson/stripe-clone/internal/ledger"
)

func TestE2EBalance(t *testing.T) {
	e := newE2E(t)

	_, empty := e.do(t, "GET", "/v1/balance", e.testKey, nil)
	if len(empty["pending"].([]any)) != 0 || empty["object"] != "balance" {
		t.Fatalf("new merchant balance = %v", empty)
	}

	charge := func(amount, currency string) string {
		pi := e.create(t, url.Values{"amount": {amount}, "currency": {currency}, "payment_method": {"pm_card_visa"}, "confirm": {"true"}})
		return pi.ID
	}
	usd1 := charge("2000", "usd")
	charge("1000", "usd")
	charge("5000", "eur")
	// Not money: declined, canceled, and another mode.
	e.call(t, "/v1/payment_intents", e.testKey, url.Values{"amount": {"900"}, "currency": {"usd"}, "payment_method": {"pm_card_chargeDeclined"}, "confirm": {"true"}}, nil)
	e.create(t, url.Values{"capture_method": {"manual"}, "payment_method": {"pm_card_visa"}, "confirm": {"true"}})

	t.Run("balance is the net of captured charges, per currency", func(t *testing.T) {
		_, b := e.do(t, "GET", "/v1/balance", e.testKey, nil)
		pending := map[string]float64{}
		for _, m := range b["pending"].([]any) {
			m := m.(map[string]any)
			pending[m["currency"].(string)] = m["amount"].(float64)
		}
		wantUSD := float64(2000 - ledger.Fee("usd", 2000) + 1000 - ledger.Fee("usd", 1000))
		wantEUR := float64(5000 - ledger.Fee("eur", 5000))
		if pending["usd"] != wantUSD || pending["eur"] != wantEUR || len(pending) != 2 {
			t.Fatalf("pending = %v, want usd %v eur %v", pending, wantUSD, wantEUR)
		}
		if b["livemode"] != false {
			t.Fatal("livemode")
		}
	})

	t.Run("live balance is separate", func(t *testing.T) {
		_, b := e.do(t, "GET", "/v1/balance", e.liveKey, nil)
		if len(b["pending"].([]any)) != 0 || b["livemode"] != true {
			t.Fatalf("live balance = %v", b)
		}
	})

	t.Run("balance transactions explain the balance", func(t *testing.T) {
		code, list := e.do(t, "GET", "/v1/balance_transactions?limit=2", e.testKey, nil)
		data := list["data"].([]any)
		if code != 200 || len(data) != 2 || list["has_more"] != true {
			t.Fatalf("list: %d %v", code, list)
		}
		last := data[1].(map[string]any)["id"].(string)
		_, rest := e.do(t, "GET", "/v1/balance_transactions?starting_after="+last, e.testKey, nil)
		all := append(data, rest["data"].([]any)...)
		if len(all) != 3 {
			t.Fatalf("%d balance transactions, want 3 charges", len(all))
		}
		oldest := all[2].(map[string]any)
		if oldest["source"] != usd1 || oldest["type"] != "charge" || oldest["amount"] != 2000.0 ||
			oldest["fee"] != float64(ledger.Fee("usd", 2000)) || oldest["net"] != float64(2000-ledger.Fee("usd", 2000)) {
			t.Fatalf("oldest = %v", oldest)
		}

		code, got := e.do(t, "GET", "/v1/balance_transactions/"+oldest["id"].(string), e.testKey, nil)
		if code != 200 || got["source"] != usd1 {
			t.Fatalf("retrieve: %d %v", code, got)
		}
		if code, _ := e.do(t, "GET", "/v1/balance_transactions/"+oldest["id"].(string), e.otherMerchantKey, nil); code != 404 {
			t.Fatalf("other merchant retrieve: %d", code)
		}
		if code, _ := e.do(t, "GET", "/v1/balance_transactions/"+oldest["id"].(string), e.liveKey, nil); code != 404 {
			t.Fatalf("live retrieve: %d", code)
		}
	})
	e.checkLedger(t)
}
