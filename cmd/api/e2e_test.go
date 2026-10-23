package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongthanhson/stripe-clone/internal/apikey"
	"github.com/truongthanhson/stripe-clone/internal/cardnetwork"
	"github.com/truongthanhson/stripe-clone/internal/idempotency"
	"github.com/truongthanhson/stripe-clone/internal/paymentintent"
	"github.com/truongthanhson/stripe-clone/internal/testdb"
)

// e2e is the real router over a real Postgres, with keys for two merchants.
type e2e struct {
	network          *cardnetwork.Fake
	pool             *pgxpool.Pool
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
	network := cardnetwork.NewFake()
	return e2e{
		network: network,
		pool:    pool,
		handler: withRequestLogging(logger, newRouter(app{
			db:      pool,
			keys:    keys,
			intents: paymentintent.NewService(paymentintent.NewStore(pool), network, 200*time.Millisecond, logger),
			idem:    idempotency.NewStore(pool),
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

// post sends a form POST with extra headers and returns the raw response.
func (e e2e) post(t *testing.T, path, key string, form url.Values, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+key)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func (e e2e) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
