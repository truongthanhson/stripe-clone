package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sync"
	"testing"

	"github.com/truongthanhson/stripe-clone/internal/ledger"
)

type refundResp struct {
	ID            string
	Amount        int64
	Status        string
	FailureReason *string `json:"failure_reason"`
	PaymentIntent string  `json:"payment_intent"`
	Error         *struct{ Code, Param string }
}

func (e e2e) refund(t *testing.T, form url.Values, headers map[string]string) (int, refundResp) {
	t.Helper()
	rec := e.post(t, "/v1/refunds", e.testKey, form, headers)
	var v refundResp
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return rec.Code, v
}

func (e e2e) pendingBalance(t *testing.T, currency string) int64 {
	t.Helper()
	_, b := e.do(t, "GET", "/v1/balance", e.testKey, nil)
	for _, m := range b["pending"].([]any) {
		if m := m.(map[string]any); m["currency"] == currency {
			return int64(m["amount"].(float64))
		}
	}
	return 0
}

func TestE2ERefunds(t *testing.T) {
	e := newE2E(t)
	charge := func(pm string) string {
		return e.create(t, url.Values{"payment_method": {pm}, "confirm": {"true"}}).ID
	}

	t.Run("full refund by default, paid by the merchant's balance", func(t *testing.T) {
		pi := charge("pm_card_visa")
		before := e.pendingBalance(t, "usd")
		code, re := e.refund(t, url.Values{"payment_intent": {pi}, "reason": {"requested_by_customer"}}, nil)
		if code != 200 || re.Status != "succeeded" || re.Amount != 2000 || re.PaymentIntent != pi {
			t.Fatalf("got %d %+v", code, re)
		}
		if got := e.pendingBalance(t, "usd"); got != before-2000 {
			t.Fatalf("pending %d -> %d, want -2000 (the fee is not returned)", before, got)
		}
		_, list := e.do(t, "GET", "/v1/balance_transactions?limit=1", e.testKey, nil)
		bt := list["data"].([]any)[0].(map[string]any)
		if bt["type"] != "refund" || bt["source"] != re.ID || bt["net"] != -2000.0 || bt["fee"] != 0.0 {
			t.Fatalf("balance transaction %v", bt)
		}
		code, again := e.refund(t, url.Values{"payment_intent": {pi}}, nil)
		if code != 400 || again.Error.Code != "charge_already_refunded" {
			t.Fatalf("second refund: %d %+v", code, again.Error)
		}
	})

	t.Run("partial refunds never exceed the amount received", func(t *testing.T) {
		pi := charge("pm_card_visa")
		for _, amount := range []string{"500", "700"} {
			if code, re := e.refund(t, url.Values{"payment_intent": {pi}, "amount": {amount}}, nil); code != 200 || re.Status != "succeeded" {
				t.Fatalf("refund %s: %d %+v", amount, code, re)
			}
		}
		code, over := e.refund(t, url.Values{"payment_intent": {pi}, "amount": {"801"}}, nil)
		if code != 400 || over.Error.Code != "amount_too_large" || over.Error.Param != "amount" {
			t.Fatalf("over-refund: %d %+v", code, over.Error)
		}
		if code, rest := e.refund(t, url.Values{"payment_intent": {pi}}, nil); code != 200 || rest.Amount != 800 {
			t.Fatalf("rest: %d %+v", code, rest)
		}
	})

	t.Run("concurrent refunds cannot over-refund", func(t *testing.T) {
		pi := charge("pm_card_visa")
		before := e.network.Stats().Refunded
		codes := make([]int, 20)
		var wg sync.WaitGroup
		for i := range codes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				codes[i] = e.post(t, "/v1/refunds", e.testKey, url.Values{"payment_intent": {pi}, "amount": {"300"}}, nil).Code
			}()
		}
		wg.Wait()
		var ok int
		for _, c := range codes {
			if c == 200 {
				ok++
			} else if c != 400 {
				t.Fatalf("unexpected status %d", c)
			}
		}
		if ok != 6 {
			t.Fatalf("%d refunds of 300 succeeded on a 2000 charge, want 6", ok)
		}
		if got := e.network.Stats().Refunded - before; got != 1800 {
			t.Fatalf("network refunded %d, want 1800", got)
		}
	})

	t.Run("a refused refund fails, moves no money, and leaves the amount refundable", func(t *testing.T) {
		pi := charge("pm_card_refundFail")
		before := e.pendingBalance(t, "usd")
		code, re := e.refund(t, url.Values{"payment_intent": {pi}, "amount": {"1000"}}, nil)
		if code != 200 || re.Status != "failed" || re.FailureReason == nil {
			t.Fatalf("got %d %+v", code, re)
		}
		if got := e.pendingBalance(t, "usd"); got != before {
			t.Fatalf("balance changed by a failed refund: %d -> %d", before, got)
		}
		if code, again := e.refund(t, url.Values{"payment_intent": {pi}, "amount": {"2000"}}, nil); code != 200 || again.Status != "failed" {
			t.Fatalf("full amount still refundable after a failure: %d %+v", code, again)
		}
	})

	t.Run("only succeeded PaymentIntents of this merchant and mode", func(t *testing.T) {
		uncaptured := e.create(t, url.Values{"capture_method": {"manual"}, "payment_method": {"pm_card_visa"}, "confirm": {"true"}})
		code, re := e.refund(t, url.Values{"payment_intent": {uncaptured.ID}}, nil)
		if code != 400 || re.Error.Code != "charge_not_refundable" {
			t.Fatalf("uncaptured: %d %+v", code, re.Error)
		}
		pi := charge("pm_card_visa")
		for _, key := range []string{e.otherMerchantKey, e.liveKey} {
			rec := e.post(t, "/v1/refunds", key, url.Values{"payment_intent": {pi}}, nil)
			if rec.Code != 404 || errorCode(t, rec.Body.Bytes()) != "resource_missing" {
				t.Fatalf("foreign refund: %d %s", rec.Code, rec.Body)
			}
		}
		if code, re := e.refund(t, url.Values{}, nil); code != 400 || re.Error.Code != "parameter_missing" {
			t.Fatalf("no payment_intent: %d %+v", code, re.Error)
		}
	})

	t.Run("a retried refund with an Idempotency-Key refunds once", func(t *testing.T) {
		pi := charge("pm_card_visa")
		key := map[string]string{"Idempotency-Key": "refund-" + pi}
		_, first := e.refund(t, url.Values{"payment_intent": {pi}, "amount": {"100"}}, key)
		_, second := e.refund(t, url.Values{"payment_intent": {pi}, "amount": {"100"}}, key)
		if first.ID == "" || first.ID != second.ID {
			t.Fatalf("first %s, retry %s", first.ID, second.ID)
		}
		_, list := e.do(t, "GET", "/v1/refunds?payment_intent="+pi, e.testKey, nil)
		if n := len(list["data"].([]any)); n != 1 {
			t.Fatalf("%d refunds for %s, want 1", n, pi)
		}
	})

	t.Run("retrieve and list", func(t *testing.T) {
		code, list := e.do(t, "GET", "/v1/refunds?limit=3", e.testKey, nil)
		if code != 200 || len(list["data"].([]any)) != 3 || list["has_more"] != true {
			t.Fatalf("list: %d %v", code, list)
		}
		id := list["data"].([]any)[0].(map[string]any)["id"].(string)
		if code, got := e.do(t, "GET", "/v1/refunds/"+id, e.testKey, nil); code != 200 || got["id"] != id {
			t.Fatalf("get: %d %v", code, got)
		}
		if code, _ := e.do(t, "GET", "/v1/refunds/"+id, e.liveKey, nil); code != 404 {
			t.Fatalf("get with live key: %d", code)
		}
	})

	t.Run("the ledger matches the network", func(t *testing.T) {
		s := e.network.Stats()
		var receivable int64
		_ = e.pool.QueryRow(t.Context(), `SELECT COALESCE(SUM(balance), 0) FROM ledger_accounts WHERE type = 'asset'`).Scan(&receivable)
		if receivable != s.Captured-s.Refunded {
			t.Fatalf("ledger receivable %d, network captured %d - refunded %d", receivable, s.Captured, s.Refunded)
		}
		var fees int64
		_ = e.pool.QueryRow(t.Context(), `SELECT COALESCE(-SUM(balance), 0) FROM ledger_accounts WHERE type = 'revenue'`).Scan(&fees)
		if fees <= 0 || fees != int64(s.Captures)*ledger.Fee("usd", 2000) {
			t.Fatalf("fees %d, want %s", fees, fmt.Sprint(s.Captures, " x ", ledger.Fee("usd", 2000)))
		}
		e.checkLedger(t)
	})
}
