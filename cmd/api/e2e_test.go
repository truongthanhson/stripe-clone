package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"testing"

	"github.com/truongthanhson/stripe-clone/internal/apikey"
	"github.com/truongthanhson/stripe-clone/internal/paymentintent"
	"github.com/truongthanhson/stripe-clone/internal/testdb"
)

// e2e is the real router over a real Postgres, with keys for two merchants.
type e2e struct {
	handler          http.Handler
	testKey, liveKey string
	otherMerchantKey string
}

func newE2E(t *testing.T) e2e {
	t.Helper()
	ctx := context.Background()
	pool := testdb.New(t)
	keys := apikey.NewStore(pool)

	issue := func(merchant int64, mode apikey.Mode) string {
		k, err := keys.Issue(ctx, merchant, mode)
		if err != nil {
			t.Fatal(err)
		}
		return k.Secret
	}
	acme, err := keys.CreateMerchant(ctx, "Acme", "ops@acme.test")
	if err != nil {
		t.Fatal(err)
	}
	globex, err := keys.CreateMerchant(ctx, "Globex", "ops@globex.test")
	if err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return e2e{
		handler: withRequestLogging(logger, newRouter(app{
			db:      pool,
			keys:    keys,
			intents: paymentintent.NewStore(pool),
		})),
		testKey:          issue(acme, apikey.ModeTest),
		liveKey:          issue(acme, apikey.ModeLive),
		otherMerchantKey: issue(globex, apikey.ModeTest),
	}
}

func (e e2e) do(t *testing.T, method, path, key string, form url.Values) (int, map[string]any) {
	return do(t, e.handler, method, path, key, form)
}

func TestE2EPaymentIntentScoping(t *testing.T) {
	e := newE2E(t)

	code, created := e.do(t, "POST", "/v1/payment_intents", e.testKey, url.Values{"amount": {"2000"}, "currency": {"usd"}})
	if code != http.StatusOK {
		t.Fatalf("create: %d %v", code, created)
	}
	path := "/v1/payment_intents/" + created["id"].(string)

	if code, body := e.do(t, "GET", path, e.testKey, nil); code != http.StatusOK || body["livemode"] != false {
		t.Fatalf("owner with test key: %d %v", code, body)
	}
	if code, _ := e.do(t, "GET", path, e.liveKey, nil); code != http.StatusNotFound {
		t.Fatalf("owner with live key: %d, want 404", code)
	}
	if code, _ := e.do(t, "GET", path, e.otherMerchantKey, nil); code != http.StatusNotFound {
		t.Fatalf("other merchant: %d, want 404", code)
	}
}
