package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/truongthanhson/stripe-clone/internal/apikey"
	"github.com/truongthanhson/stripe-clone/internal/paymentintent"
)

// fakeIntents is an in-memory intentStore that scopes reads like the real one.
type fakeIntents map[string]paymentintent.PaymentIntent

func (f fakeIntents) Create(_ context.Context, merchantID int64, livemode bool, p paymentintent.CreateParams) (paymentintent.PaymentIntent, error) {
	if err := p.Validate(); err != nil {
		return paymentintent.PaymentIntent{}, err
	}
	pi := paymentintent.PaymentIntent{
		ID: paymentintent.NewID(), MerchantID: merchantID, Livemode: livemode,
		Amount: p.Amount, Currency: p.Currency, Status: p.InitialStatus(), CaptureMethod: p.CaptureMethod,
		Description: p.Description, PaymentMethod: p.PaymentMethod, Created: time.Unix(1_700_000_000, 0),
	}
	f[pi.ID] = pi
	return pi, nil
}

func (f fakeIntents) Get(_ context.Context, merchantID int64, livemode bool, id string) (paymentintent.PaymentIntent, error) {
	pi, ok := f[id]
	if !ok || pi.MerchantID != merchantID || pi.Livemode != livemode {
		return paymentintent.PaymentIntent{}, paymentintent.ErrNotFound
	}
	return pi, nil
}

var testKeys = fakeKeys{
	"sk_test_good": {MerchantID: 7, Mode: apikey.ModeTest},
	"sk_live_good": {MerchantID: 7, Mode: apikey.ModeLive},
}

func do(t *testing.T, h http.Handler, method, path, key string, form url.Values) (int, map[string]any) {
	t.Helper()
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %q", rec.Body)
	}
	return rec.Code, out
}

func errorOf(body map[string]any) map[string]any {
	e, _ := body["error"].(map[string]any)
	return e
}

func TestCreateIntent(t *testing.T) {
	router := newRouter(app{keys: testKeys, intents: fakeIntents{}})

	code, body := do(t, router, "POST", "/v1/payment_intents", "sk_test_good", url.Values{
		"amount": {"2000"}, "currency": {"USD"}, "description": {"Order #1"},
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d: %v", code, body)
	}
	want := map[string]any{
		"object": "payment_intent", "amount": 2000.0, "currency": "usd", "status": "requires_payment_method",
		"capture_method": "automatic", "livemode": false, "description": "Order #1", "payment_method": nil,
	}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("%s = %v, want %v", k, body[k], v)
		}
	}
	if id, _ := body["id"].(string); !strings.HasPrefix(id, "pi_") {
		t.Errorf("id = %v, want pi_ prefix", body["id"])
	}

	code, body = do(t, router, "POST", "/v1/payment_intents", "sk_test_good", url.Values{
		"amount": {"2000"}, "currency": {"usd"}, "payment_method": {"pm_card_visa"}, "capture_method": {"manual"},
	})
	if code != http.StatusOK || body["status"] != "requires_confirmation" || body["capture_method"] != "manual" {
		t.Fatalf("with payment method: %d %v", code, body)
	}
}

func TestCreateIntentErrors(t *testing.T) {
	router := newRouter(app{keys: testKeys, intents: fakeIntents{}})

	tests := []struct {
		name      string
		form      url.Values
		wantCode  string
		wantParam string
	}{
		{"missing amount", url.Values{"currency": {"usd"}}, "parameter_missing", "amount"},
		{"missing currency", url.Values{"amount": {"2000"}}, "parameter_missing", "currency"},
		{"amount not an integer", url.Values{"amount": {"20.00"}, "currency": {"usd"}}, "parameter_invalid_integer", "amount"},
		{"amount too small", url.Values{"amount": {"1"}, "currency": {"usd"}}, "amount_too_small", "amount"},
		{"unsupported currency", url.Values{"amount": {"2000"}, "currency": {"abc"}}, "currency_unsupported", "currency"},
		{"unknown parameter", url.Values{"amount": {"2000"}, "currency": {"usd"}, "amout": {"1"}}, "parameter_unknown", "amout"},
		{"bad payment method", url.Values{"amount": {"2000"}, "currency": {"usd"}, "payment_method": {"4242424242424242"}}, "resource_missing", "payment_method"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, body := do(t, router, "POST", "/v1/payment_intents", "sk_test_good", tt.form)
			e := errorOf(body)
			if code != http.StatusBadRequest || e["type"] != "invalid_request_error" || e["code"] != tt.wantCode || e["param"] != tt.wantParam {
				t.Fatalf("got %d %v, want 400 code %q param %q", code, body, tt.wantCode, tt.wantParam)
			}
		})
	}

	t.Run("JSON body is refused with a hint", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/v1/payment_intents", strings.NewReader(`{"amount":2000}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer sk_test_good")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "content_type_invalid") {
			t.Fatalf("got %d %s", rec.Code, rec.Body)
		}
	})
}

func TestGetIntent(t *testing.T) {
	intents := fakeIntents{}
	router := newRouter(app{keys: testKeys, intents: intents})

	_, created := do(t, router, "POST", "/v1/payment_intents", "sk_test_good", url.Values{"amount": {"2000"}, "currency": {"usd"}})
	id := created["id"].(string)

	code, body := do(t, router, "GET", "/v1/payment_intents/"+id, "sk_test_good", nil)
	if code != http.StatusOK || body["id"] != id {
		t.Fatalf("get: %d %v", code, body)
	}

	code, body = do(t, router, "GET", "/v1/payment_intents/"+id, "sk_live_good", nil)
	if e := errorOf(body); code != http.StatusNotFound || e["code"] != "resource_missing" {
		t.Fatalf("get with live key: %d %v, want 404 resource_missing", code, body)
	}
}
