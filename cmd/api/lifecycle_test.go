package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sync"
	"testing"
)

// intentResp is the subset of a PaymentIntent or error response these tests read.
type intentResp struct {
	ID               string
	Status           string
	Amount           int64
	AmountCapturable int64   `json:"amount_capturable"`
	AmountReceived   int64   `json:"amount_received"`
	PaymentMethod    *string `json:"payment_method"`
	LastPaymentError *struct {
		Code        string
		DeclineCode string `json:"decline_code"`
	} `json:"last_payment_error"`
	CancellationReason *string `json:"cancellation_reason"`
	Error              *struct {
		Type          string
		Code          string
		DeclineCode   string `json:"decline_code"`
		Param         string
		PaymentIntent *intentResp `json:"payment_intent"`
	}
}

func (e e2e) call(t *testing.T, path, key string, form url.Values, headers map[string]string) (int, intentResp) {
	t.Helper()
	rec := e.post(t, path, key, form, headers)
	var v intentResp
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return rec.Code, v
}

func (e e2e) create(t *testing.T, form url.Values) intentResp {
	t.Helper()
	if !form.Has("amount") {
		form.Set("amount", "2000")
	}
	if !form.Has("currency") {
		form.Set("currency", "usd")
	}
	code, pi := e.call(t, "/v1/payment_intents", e.testKey, form, nil)
	if code != http.StatusOK {
		t.Fatalf("create: %d %+v", code, pi.Error)
	}
	return pi
}

func wantState(t *testing.T, code int, pi intentResp, wantCode int, wantStatus string) {
	t.Helper()
	if code != wantCode || pi.Status != wantStatus {
		t.Fatalf("got %d status %q (error %+v), want %d %q", code, pi.Status, pi.Error, wantCode, wantStatus)
	}
}

func wantError(t *testing.T, code int, pi intentResp, wantCode int, wantErrCode string) {
	t.Helper()
	if code != wantCode || pi.Error == nil || pi.Error.Code != wantErrCode {
		t.Fatalf("got %d %+v, want %d %q", code, pi.Error, wantCode, wantErrCode)
	}
}

func TestE2ELifecycle(t *testing.T) {
	e := newE2E(t)
	path := func(id, action string) string { return "/v1/payment_intents/" + id + "/" + action }

	t.Run("automatic capture: confirm charges the card", func(t *testing.T) {
		before := e.network.Stats()
		pi := e.create(t, url.Values{"payment_method": {"pm_card_visa"}, "confirm": {"true"}})
		if pi.Status != "succeeded" || pi.AmountReceived != 2000 || pi.AmountCapturable != 0 {
			t.Fatalf("got %+v", pi)
		}
		if got := e.network.Stats().Captured - before.Captured; got != 2000 {
			t.Fatalf("network captured %d, want 2000", got)
		}
	})

	t.Run("manual capture: authorize, then capture part of it", func(t *testing.T) {
		pi := e.create(t, url.Values{"capture_method": {"manual"}})
		if pi.Status != "requires_payment_method" {
			t.Fatalf("status %q", pi.Status)
		}
		code, pi := e.call(t, path(pi.ID, "confirm"), e.testKey, url.Values{"payment_method": {"pm_card_visa"}}, nil)
		wantState(t, code, pi, 200, "requires_capture")
		if pi.AmountCapturable != 2000 || pi.AmountReceived != 0 {
			t.Fatalf("after confirm %+v", pi)
		}

		code, over := e.call(t, path(pi.ID, "capture"), e.testKey, url.Values{"amount_to_capture": {"2001"}}, nil)
		wantError(t, code, over, 400, "amount_too_large")

		code, pi = e.call(t, path(pi.ID, "capture"), e.testKey, url.Values{"amount_to_capture": {"1500"}}, nil)
		wantState(t, code, pi, 200, "succeeded")
		if pi.AmountReceived != 1500 || pi.AmountCapturable != 0 {
			t.Fatalf("after capture %+v", pi)
		}

		code, again := e.call(t, path(pi.ID, "capture"), e.testKey, url.Values{}, nil)
		wantError(t, code, again, 400, "payment_intent_unexpected_state")
	})

	t.Run("decline returns 402 and the PaymentIntent can be retried with another card", func(t *testing.T) {
		pi := e.create(t, url.Values{"payment_method": {"pm_card_chargeDeclinedInsufficientFunds"}})
		code, declined := e.call(t, path(pi.ID, "confirm"), e.testKey, url.Values{}, nil)
		wantError(t, code, declined, http.StatusPaymentRequired, "card_declined")
		if declined.Error.Type != "card_error" || declined.Error.DeclineCode != "insufficient_funds" {
			t.Fatalf("error %+v", declined.Error)
		}
		after := declined.Error.PaymentIntent
		if after == nil || after.Status != "requires_payment_method" || after.PaymentMethod != nil ||
			after.LastPaymentError == nil || after.LastPaymentError.DeclineCode != "insufficient_funds" {
			t.Fatalf("payment_intent in error = %+v", after)
		}

		code, pi = e.call(t, path(pi.ID, "confirm"), e.testKey, url.Values{"payment_method": {"pm_card_visa"}}, nil)
		wantState(t, code, pi, 200, "succeeded")
		if pi.LastPaymentError != nil {
			t.Fatal("last_payment_error not cleared after success")
		}
	})

	t.Run("network timeout leaves the PaymentIntent processing, never guessed", func(t *testing.T) {
		before := e.network.Stats()
		pi := e.create(t, url.Values{"payment_method": {"pm_card_timeout"}})
		code, pi := e.call(t, path(pi.ID, "confirm"), e.testKey, url.Values{}, nil)
		wantState(t, code, pi, 200, "processing")
		if got := e.network.Stats().Holds - before.Holds; got != 1 {
			t.Fatalf("network holds = %d, want 1 (the network approved; we just never heard)", got)
		}
		code, pi = e.call(t, path(pi.ID, "cancel"), e.testKey, url.Values{}, nil)
		wantError(t, code, pi, 400, "payment_intent_unexpected_state")
	})

	t.Run("cancel voids an uncaptured authorization", func(t *testing.T) {
		before := e.network.Stats()
		pi := e.create(t, url.Values{"capture_method": {"manual"}, "payment_method": {"pm_card_visa"}, "confirm": {"true"}})
		code, pi := e.call(t, path(pi.ID, "cancel"), e.testKey, url.Values{"cancellation_reason": {"requested_by_customer"}}, nil)
		wantState(t, code, pi, 200, "canceled")
		if pi.CancellationReason == nil || *pi.CancellationReason != "requested_by_customer" || pi.AmountCapturable != 0 {
			t.Fatalf("after cancel %+v", pi)
		}
		if got := e.network.Stats().Captures - before.Captures; got != 0 {
			t.Fatalf("network captures = %d, want 0", got)
		}
		code, again := e.call(t, path(pi.ID, "capture"), e.testKey, url.Values{}, nil)
		wantError(t, code, again, 400, "payment_intent_unexpected_state")
		code, again = e.call(t, path(pi.ID, "confirm"), e.testKey, url.Values{}, nil)
		wantError(t, code, again, 400, "payment_intent_unexpected_state")
	})

	t.Run("succeeded cannot be canceled or confirmed again", func(t *testing.T) {
		pi := e.create(t, url.Values{"payment_method": {"pm_card_visa"}, "confirm": {"true"}})
		code, got := e.call(t, path(pi.ID, "cancel"), e.testKey, url.Values{}, nil)
		wantError(t, code, got, 400, "payment_intent_unexpected_state")
		code, got = e.call(t, path(pi.ID, "confirm"), e.testKey, url.Values{}, nil)
		wantError(t, code, got, 400, "payment_intent_unexpected_state")
	})

	t.Run("confirm input errors", func(t *testing.T) {
		pi := e.create(t, url.Values{})
		code, got := e.call(t, path(pi.ID, "confirm"), e.testKey, url.Values{}, nil)
		wantError(t, code, got, 400, "parameter_missing")
		code, got = e.call(t, path(pi.ID, "confirm"), e.testKey, url.Values{"payment_method": {"pm_card_nope"}}, nil)
		wantError(t, code, got, 400, "resource_missing")
		code, got = e.call(t, path(pi.ID, "cancel"), e.testKey, url.Values{"cancellation_reason": {"bored"}}, nil)
		wantError(t, code, got, 400, "parameter_invalid_string")
		code, got = e.call(t, path("pi_missing", "confirm"), e.testKey, url.Values{}, nil)
		wantError(t, code, got, 404, "resource_missing")
	})

	t.Run("live mode cannot charge until a live processor exists", func(t *testing.T) {
		code, pi := e.call(t, "/v1/payment_intents", e.liveKey, url.Values{"amount": {"2000"}, "currency": {"usd"}}, nil)
		wantState(t, code, pi, 200, "requires_payment_method")
		code, pi = e.call(t, path(pi.ID, "confirm"), e.liveKey, url.Values{"payment_method": {"pm_card_visa"}}, nil)
		wantError(t, code, pi, 400, "livemode_unavailable")
	})
}

func TestE2ECaptureRacesCancel(t *testing.T) {
	e := newE2E(t)
	for range 10 {
		before := e.network.Stats()
		pi := e.create(t, url.Values{"capture_method": {"manual"}, "payment_method": {"pm_card_visa"}, "confirm": {"true"}})

		var wg sync.WaitGroup
		codes := make([]int, 2)
		for i, action := range []string{"capture", "cancel"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				codes[i] = e.post(t, "/v1/payment_intents/"+pi.ID+"/"+action, e.testKey, url.Values{}, nil).Code
			}()
		}
		wg.Wait()

		_, final := e.do(t, "GET", "/v1/payment_intents/"+pi.ID, e.testKey, nil)
		captured := e.network.Stats().Captures - before.Captures
		switch final["status"] {
		case "succeeded":
			if captured != 1 || codes[0] != 200 || codes[1] != 400 {
				t.Fatalf("succeeded with %d captures, codes %v", captured, codes)
			}
		case "canceled":
			if captured != 0 || codes[0] != 400 || codes[1] != 200 {
				t.Fatalf("canceled with %d captures, codes %v", captured, codes)
			}
		default:
			t.Fatalf("final status %v", final["status"])
		}
	}
}

// TestE2EPhase1Gate is the Phase 1 gate: 50 concurrent retries with the same Idempotency-Key
// produce exactly one charge on the card network.
func TestE2EPhase1Gate(t *testing.T) {
	e := newE2E(t)
	const n = 50

	run := func(t *testing.T, path string, form url.Values, key string) []int {
		codes := make([]int, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var h map[string]string
				if key != "" {
					h = map[string]string{"Idempotency-Key": key}
				}
				codes[i] = e.post(t, path, e.testKey, form, h).Code
			}()
		}
		wg.Wait()
		return codes
	}
	count := func(codes []int, code int) int {
		var c int
		for _, x := range codes {
			if x == code {
				c++
			}
		}
		return c
	}

	t.Run("create-and-confirm retried 50 times with one key", func(t *testing.T) {
		before := e.network.Stats()
		codes := run(t, "/v1/payment_intents",
			url.Values{"amount": {"7777"}, "currency": {"usd"}, "payment_method": {"pm_card_visa"}, "confirm": {"true"}}, "gate-create")
		if count(codes, 200)+count(codes, 409) != n || count(codes, 200) == 0 {
			t.Fatalf("codes %v", codes)
		}
		after := e.network.Stats()
		if after.Captures-before.Captures != 1 || after.Captured-before.Captured != 7777 || after.Holds-before.Holds != 1 {
			t.Fatalf("network moved %d in %d captures (%d holds), want exactly one charge of 7777",
				after.Captured-before.Captured, after.Captures-before.Captures, after.Holds-before.Holds)
		}
		if got := e.count(t, `SELECT count(*) FROM payment_intents WHERE amount = 7777`); got != 1 {
			t.Fatalf("%d PaymentIntents, want 1", got)
		}
	})

	t.Run("confirm retried 50 times with one key", func(t *testing.T) {
		pi := e.create(t, url.Values{"payment_method": {"pm_card_visa"}})
		before := e.network.Stats()
		codes := run(t, "/v1/payment_intents/"+pi.ID+"/confirm", url.Values{}, "gate-confirm")
		if count(codes, 200)+count(codes, 409) != n {
			t.Fatalf("codes %v", codes)
		}
		if after := e.network.Stats(); after.Captures-before.Captures != 1 {
			t.Fatalf("%d charges, want 1", after.Captures-before.Captures)
		}
	})

	t.Run("confirm sent 50 times without a key: the state machine still allows one charge", func(t *testing.T) {
		pi := e.create(t, url.Values{"payment_method": {"pm_card_visa"}})
		before := e.network.Stats()
		codes := run(t, "/v1/payment_intents/"+pi.ID+"/confirm", url.Values{}, "")
		if count(codes, 200) != 1 || count(codes, 400) != n-1 {
			t.Fatalf("want one 200 and %d x 400, got %v", n-1, codes)
		}
		if after := e.network.Stats(); after.Captures-before.Captures != 1 {
			t.Fatalf("%d charges, want 1", after.Captures-before.Captures)
		}
	})
}
