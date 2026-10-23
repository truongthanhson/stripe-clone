package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type sessionResp struct {
	ID              string                   `json:"id"`
	Status          string                   `json:"status"`
	PaymentStatus   string                   `json:"payment_status"`
	PaymentIntent   string                   `json:"payment_intent"`
	AmountTotal     int64                    `json:"amount_total"`
	Currency        string                   `json:"currency"`
	URL             *string                  `json:"url"`
	CustomerDetails *struct{ Email *string } `json:"customer_details"`
	Metadata        map[string]string        `json:"metadata"`
}

func decodeInto(t *testing.T, body []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
}

func sessionForm(extra ...string) url.Values {
	f := url.Values{
		"mode":                                   {"payment"},
		"success_url":                            {"https://shop.example.test/thanks?session={CHECKOUT_SESSION_ID}"},
		"cancel_url":                             {"https://shop.example.test/cart"},
		"line_items[0][price_data][currency]":    {"usd"},
		"line_items[0][price_data][unit_amount]": {"1500"},
		"line_items[0][price_data][product_data][name]": {"T-shirt"},
		"line_items[0][quantity]":                       {"2"},
		"line_items[1][price_data][currency]":           {"usd"},
		"line_items[1][price_data][unit_amount]":        {"500"},
		"line_items[1][price_data][product_data][name]": {"Sticker"},
		"line_items[1][quantity]":                       {"1"},
	}
	for i := 0; i+1 < len(extra); i += 2 {
		if extra[i+1] == "" {
			f.Del(extra[i])
		} else {
			f.Set(extra[i], extra[i+1])
		}
	}
	return f
}

func (e e2e) createSession(t *testing.T, key string, form url.Values) sessionResp {
	t.Helper()
	rec := e.post(t, "/v1/checkout/sessions", key, form, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("create session: %d %s", rec.Code, rec.Body)
	}
	var s sessionResp
	decodeInto(t, rec.Body.Bytes(), &s)
	return s
}

func (e e2e) getSession(t *testing.T, id string) sessionResp {
	t.Helper()
	req := httptest.NewRequest("GET", "/v1/checkout/sessions/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+e.testKey)
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get session: %d %s", rec.Code, rec.Body)
	}
	var s sessionResp
	decodeInto(t, rec.Body.Bytes(), &s)
	return s
}

// page sends a browser request to the hosted page, with no API key.
func (e e2e) page(t *testing.T, method, id string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if form != nil {
		req = httptest.NewRequest(method, "/c/pay/"+id, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, "/c/pay/"+id, nil)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func card(number string) url.Values {
	return url.Values{"email": {"buyer@example.test"}, "number": {number}, "exp": {"12 / 34"}, "cvc": {"123"}}
}

func TestE2ECheckoutCreate(t *testing.T) {
	e := newE2E(t)

	s := e.createSession(t, e.testKey, sessionForm("metadata[order]", "42", "client_reference_id", "cart-9"))
	if !strings.HasPrefix(s.ID, "cs_test_") || s.Status != "open" || s.PaymentStatus != "unpaid" || s.AmountTotal != 3500 || s.Currency != "usd" {
		t.Fatalf("session: %+v", s)
	}
	if s.URL == nil || *s.URL != "https://pay.example.test/c/pay/"+s.ID {
		t.Fatalf("url: %v", s.URL)
	}
	if s.Metadata["order"] != "42" {
		t.Fatalf("metadata: %v", s.Metadata)
	}
	code, pi := e.do(t, "GET", "/v1/payment_intents/"+s.PaymentIntent, e.testKey, nil)
	if code != 200 || pi["amount"] != float64(3500) || pi["status"] != "requires_payment_method" {
		t.Fatalf("payment intent: %d %v", code, pi)
	}

	code, items := e.do(t, "GET", "/v1/checkout/sessions/"+s.ID+"/line_items", e.testKey, nil)
	data, _ := items["data"].([]any)
	if code != 200 || len(data) != 2 || data[0].(map[string]any)["amount_total"] != float64(3000) || data[1].(map[string]any)["description"] != "Sticker" {
		t.Fatalf("line items: %d %v", code, items)
	}

	code, list := e.do(t, "GET", "/v1/checkout/sessions?limit=1", e.testKey, nil)
	if data, _ := list["data"].([]any); code != 200 || len(data) != 1 || data[0].(map[string]any)["id"] != s.ID {
		t.Fatalf("list: %d %v", code, list)
	}

	if code, _ := e.do(t, "GET", "/v1/checkout/sessions/"+s.ID, e.otherMerchantKey, nil); code != 404 {
		t.Fatalf("other merchant reads session: %d, want 404", code)
	}
	if code, _ := e.do(t, "GET", "/v1/checkout/sessions/"+s.ID, e.liveKey, nil); code != 404 {
		t.Fatalf("live key reads test session: %d, want 404", code)
	}
}

func TestE2ECheckoutCreateErrors(t *testing.T) {
	e := newE2E(t)
	soon := time.Now().Add(10 * time.Minute).Unix()
	tests := []struct {
		name      string
		form      url.Values
		wantParam string
	}{
		{"no line items", sessionForm(
			"line_items[0][price_data][currency]", "", "line_items[0][price_data][unit_amount]", "", "line_items[0][price_data][product_data][name]", "", "line_items[0][quantity]", "",
			"line_items[1][price_data][currency]", "", "line_items[1][price_data][unit_amount]", "", "line_items[1][price_data][product_data][name]", "", "line_items[1][quantity]", ""),
			"line_items"},
		{"missing mode", sessionForm("mode", ""), "mode"},
		{"script url", sessionForm("success_url", "javascript:alert(1)"), "success_url"},
		{"relative url", sessionForm("cancel_url", "/cart"), "cancel_url"},
		{"mixed currencies", sessionForm("line_items[1][price_data][currency]", "eur"), "line_items[1][price_data][currency]"},
		{"unknown item field", sessionForm("line_items[0][price]", "price_1"), "line_items[0][price]"},
		{"gap in indexes", sessionForm("line_items[3][quantity]", "1"), "line_items[2]"},
		{"zero quantity", sessionForm("line_items[0][quantity]", "0"), "line_items[0][quantity]"},
		{"missing quantity", sessionForm("line_items[0][quantity]", ""), "line_items[0][quantity]"},
		{"unsupported currency", sessionForm("line_items[0][price_data][currency]", "xyz", "line_items[1][price_data][currency]", "xyz"), "line_items[0][price_data][currency]"},
		{"total too small", sessionForm("line_items[0][price_data][unit_amount]", "0", "line_items[1][price_data][unit_amount]", "10"), "line_items"},
		{"customer and email", sessionForm("customer", "cus_x", "customer_email", "a@b.test"), "customer_email"},
		{"no such customer", sessionForm("customer", "cus_missing"), "customer"},
		{"expires too soon", sessionForm("expires_at", strconv.FormatInt(soon, 10)), "expires_at"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, body := e.do(t, "POST", "/v1/checkout/sessions", e.testKey, tt.form)
			if code != 400 || errorOf(body)["param"] != tt.wantParam {
				t.Fatalf("got %d %v, want 400 param %s", code, body, tt.wantParam)
			}
		})
	}
	if n := e.count(t, `SELECT count(*) FROM payment_intents`); n != 0 {
		t.Fatalf("%d PaymentIntents left behind by rejected sessions", n)
	}
}

func TestE2ECheckoutPage(t *testing.T) {
	e := newE2E(t)
	s := e.createSession(t, e.testKey, sessionForm("customer_email", "jenny@example.test"))

	rec := e.page(t, "GET", s.ID, nil)
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "Acme") || !strings.Contains(body, "$35.00") || !strings.Contains(body, "jenny@example.test") {
		t.Fatalf("page: %d\n%s", rec.Code, body)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") || rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatalf("security headers: %v", rec.Header())
	}
	if rec := e.page(t, "GET", "cs_test_missing", nil); rec.Code != 404 {
		t.Fatalf("missing session: %d", rec.Code)
	}

	t.Run("form mistakes charge nothing", func(t *testing.T) {
		for _, f := range []url.Values{
			card("4242424242424241"), // fails the checksum
			card("4111111111111111"), // a valid number, but not a test card
			{"email": {"a@b.test"}, "number": {"4242424242424242"}, "exp": {"01 / 20"}, "cvc": {"123"}},
			{"email": {"a@b.test"}, "number": {"4242424242424242"}, "exp": {"12 / 34"}, "cvc": {"1"}},
			{"email": {"nope"}, "number": {"4242424242424242"}, "exp": {"12 / 34"}, "cvc": {"123"}},
		} {
			if rec := e.page(t, "POST", s.ID, f); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `role="alert"`) {
				t.Fatalf("%v: %d", f, rec.Code)
			}
		}
		if got := e.network.Stats().AuthorizeCalls; got != 0 {
			t.Fatalf("network called %d times for invalid forms", got)
		}
	})

	t.Run("decline, then pay with another card", func(t *testing.T) {
		rec := e.page(t, "POST", s.ID, card("4000 0000 0000 0002"))
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/c/pay/"+s.ID {
			t.Fatalf("decline: %d %v", rec.Code, rec.Header())
		}
		if body := e.page(t, "GET", s.ID, nil).Body.String(); !strings.Contains(body, "Your card was declined.") {
			t.Fatalf("decline message missing:\n%s", body)
		}
		if got := e.getSession(t, s.ID); got.Status != "open" {
			t.Fatalf("after decline: %+v", got)
		}

		rec = e.page(t, "POST", s.ID, card("4242 4242 4242 4242"))
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "https://shop.example.test/thanks?session="+s.ID {
			t.Fatalf("pay: %d %v", rec.Code, rec.Header())
		}
		got := e.getSession(t, s.ID)
		if got.Status != "complete" || got.PaymentStatus != "paid" || got.URL != nil || got.CustomerDetails == nil || *got.CustomerDetails.Email != "buyer@example.test" {
			t.Fatalf("after pay: %+v", got)
		}
		if charged := e.charged(t, s.PaymentIntent); charged != 3500 {
			t.Fatalf("ledger charged %d, want 3500", charged)
		}
		e.checkLedger(t)
	})

	t.Run("a paid session cannot be paid again", func(t *testing.T) {
		before := e.network.Stats().AuthorizeCalls
		rec := e.page(t, "POST", s.ID, card("4242424242424242"))
		if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "https://shop.example.test/thanks") {
			t.Fatalf("second pay: %d %v", rec.Code, rec.Header())
		}
		if e.network.Stats().AuthorizeCalls != before {
			t.Fatal("a completed session reached the network again")
		}
		if body := e.page(t, "GET", s.ID, nil).Body.String(); !strings.Contains(body, "Payment successful") {
			t.Fatalf("completed page:\n%s", body)
		}
	})
}

func TestE2ECheckoutExpiry(t *testing.T) {
	e := newE2E(t)

	t.Run("expire through the API", func(t *testing.T) {
		s := e.createSession(t, e.testKey, sessionForm())
		code, body := e.do(t, "POST", "/v1/checkout/sessions/"+s.ID+"/expire", e.testKey, nil)
		if code != 200 || body["status"] != "expired" || body["url"] != nil {
			t.Fatalf("expire: %d %v", code, body)
		}
		_, pi := e.do(t, "GET", "/v1/payment_intents/"+s.PaymentIntent, e.testKey, nil)
		if pi["status"] != "canceled" || pi["cancellation_reason"] != "abandoned" {
			t.Fatalf("payment intent: %v", pi)
		}
		if code, body := e.do(t, "POST", "/v1/checkout/sessions/"+s.ID+"/expire", e.testKey, nil); code != 400 || errorOf(body)["code"] != "checkout_session_unexpected_state" {
			t.Fatalf("expire twice: %d %v", code, body)
		}
		rec := e.page(t, "POST", s.ID, card("4242424242424242"))
		if rec.Code != http.StatusSeeOther || e.network.Stats().AuthorizeCalls != 0 {
			t.Fatalf("paying an expired session: %d, %d network calls", rec.Code, e.network.Stats().AuthorizeCalls)
		}
		if body := e.page(t, "GET", s.ID, nil).Body.String(); !strings.Contains(body, "has expired") {
			t.Fatalf("expired page:\n%s", body)
		}
	})

	t.Run("expires on its own after expires_at", func(t *testing.T) {
		s := e.createSession(t, e.testKey, sessionForm())
		if _, err := e.pool.Exec(context.Background(), `UPDATE checkout_sessions SET expires_at = now() - interval '1 second' WHERE id = $1`, s.ID); err != nil {
			t.Fatal(err)
		}
		if got := e.getSession(t, s.ID); got.Status != "expired" {
			t.Fatalf("past expires_at: %+v", got)
		}
	})

	t.Run("a payment in progress cannot be expired", func(t *testing.T) {
		s := e.createSession(t, e.testKey, sessionForm())
		rec := e.page(t, "POST", s.ID, card("4000000000009003")) // the bank never answers
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("timeout pay: %d", rec.Code)
		}
		page := e.page(t, "GET", s.ID, nil).Body.String()
		if !strings.Contains(page, "Processing your payment") || !strings.Contains(page, `http-equiv="refresh"`) {
			t.Fatalf("processing page:\n%s", page)
		}
		if code, body := e.do(t, "POST", "/v1/checkout/sessions/"+s.ID+"/expire", e.testKey, nil); code != 400 {
			t.Fatalf("expire while processing: %d %v", code, body)
		}
		if _, err := e.pool.Exec(context.Background(), `UPDATE checkout_sessions SET expires_at = now() - interval '1 second' WHERE id = $1`, s.ID); err != nil {
			t.Fatal(err)
		}
		if got := e.getSession(t, s.ID); got.Status != "open" {
			t.Fatalf("a processing payment must not be expired by time either: %+v", got)
		}
	})
}

// TestE2ECheckoutGate is the hosted checkout's guarantee: however many times the buyer submits, and
// even when an expiry races the payment, the card is charged at most once and the session's status
// always matches its PaymentIntent.
func TestE2ECheckoutGate(t *testing.T) {
	e := newE2E(t)

	t.Run("20 concurrent submits charge once", func(t *testing.T) {
		s := e.createSession(t, e.testKey, sessionForm())
		var wg sync.WaitGroup
		codes := make([]int, 20)
		for i := range codes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				codes[i] = e.page(t, "POST", s.ID, card("4242424242424242")).Code
			}()
		}
		wg.Wait()
		for _, c := range codes {
			if c != http.StatusSeeOther {
				t.Fatalf("codes: %v", codes)
			}
		}
		if got := e.getSession(t, s.ID); got.Status != "complete" {
			t.Fatalf("session: %+v", got)
		}
		if charged := e.charged(t, s.PaymentIntent); charged != 3500 {
			t.Fatalf("charged %d, want 3500 once", charged)
		}
	})

	t.Run("expiry racing payment never charges an expired session", func(t *testing.T) {
		for range 20 {
			s := e.createSession(t, e.testKey, sessionForm())
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); e.page(t, "POST", s.ID, card("4242424242424242")) }()
			go func() { defer wg.Done(); e.post(t, "/v1/checkout/sessions/"+s.ID+"/expire", e.testKey, nil, nil) }()
			wg.Wait()

			got := e.getSession(t, s.ID)
			charged := e.charged(t, s.PaymentIntent)
			switch {
			case got.Status == "complete" && charged == 3500:
			case got.Status == "expired" && charged == 0:
			default:
				t.Fatalf("session %s is %s with %d charged", s.ID, got.Status, charged)
			}
		}
		e.checkLedger(t)
	})
}

func TestE2ECheckoutAttemptsLimit(t *testing.T) {
	e := newE2E(t)
	s := e.createSession(t, e.testKey, sessionForm())
	for range 10 {
		if rec := e.page(t, "POST", s.ID, card("4000000000000002")); rec.Code != http.StatusSeeOther {
			t.Fatalf("decline: %d", rec.Code)
		}
	}
	before := e.network.Stats().AuthorizeCalls
	rec := e.page(t, "POST", s.ID, card("4242424242424242"))
	if rec.Code != http.StatusTooManyRequests || e.network.Stats().AuthorizeCalls != before {
		t.Fatalf("11th attempt: %d, network calls %d -> %d", rec.Code, before, e.network.Stats().AuthorizeCalls)
	}
}

func TestE2ECheckoutLivemode(t *testing.T) {
	e := newE2E(t)
	s := e.createSession(t, e.liveKey, sessionForm())
	if !strings.HasPrefix(s.ID, "cs_live_") {
		t.Fatalf("id: %s", s.ID)
	}
	rec := e.page(t, "POST", s.ID, card("4242424242424242"))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Live payments are not available yet") {
		t.Fatalf("live pay: %d\n%s", rec.Code, rec.Body)
	}
	if body := e.page(t, "GET", s.ID, nil).Body.String(); strings.Contains(body, "Test mode") || strings.Contains(body, "Test cards") {
		t.Fatal("live page shows test mode hints")
	}
}
