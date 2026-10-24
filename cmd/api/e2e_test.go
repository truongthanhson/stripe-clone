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
	"github.com/truongthanhson/stripe-clone/internal/ledger"
	"github.com/truongthanhson/stripe-clone/internal/testdb"
)

// e2e is the real router over a real Postgres, with keys for two merchants.
type e2e struct {
	network          *cardnetwork.Fake
	resolver         resolver
	pool             *pgxpool.Pool
	handler          http.Handler
	testKey, liveKey string
	otherMerchantKey string
}

func newE2E(t *testing.T) e2e { return newE2EWith(t, "", nil) }

func newE2EAtVersion(t *testing.T, version string) e2e { return newE2EWith(t, version, nil) }

// newE2EWith sends every request with Stripe-Version: version (none when empty), which is how
// an integration written against an older version talks to today's server. link, when set, stands
// between the server and the fake bank, as a lossy network does in the recovery test.
func newE2EWith(t *testing.T, version string, link func(cardnetwork.Network) cardnetwork.Network) e2e {
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
	var bank cardnetwork.Network = network
	if link != nil {
		bank = link(network)
	}
	a := buildApp(pool, bank, 200*time.Millisecond, logger, "https://pay.example.test")
	return e2e{
		network:          network,
		pool:             pool,
		resolver:         a.resolver,
		handler:          withVersionHeader(version, withContractCheck(t, withRequestLogging(logger, newRouter(a)))),
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

func withVersionHeader(version string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if version != "" && r.Header.Get("Stripe-Version") == "" {
			r.Header.Set("Stripe-Version", version)
		}
		next.ServeHTTP(w, r)
	})
}

// checkLedger fails the test if any ledger invariant is broken.
func (e e2e) checkLedger(t *testing.T) {
	t.Helper()
	problems, err := ledger.Check(context.Background(), e.pool)
	if err != nil || len(problems) > 0 {
		t.Fatalf("ledger check: %v %v", err, problems)
	}
}

// charged returns the gross amount the ledger recorded as charged for a PaymentIntent, 0 if none.
func (e e2e) charged(t *testing.T, piID string) int64 {
	t.Helper()
	var gross int64
	err := e.pool.QueryRow(context.Background(), `
		SELECT COALESCE(SUM(e.amount), 0) FROM ledger_entries e
		JOIN ledger_transactions t ON t.id = e.transaction_id
		JOIN ledger_accounts a ON a.id = e.account_id
		WHERE t.type = 'charge' AND t.source_id = $1 AND a.type = 'asset'`, piID).Scan(&gross)
	if err != nil {
		t.Fatal(err)
	}
	return gross
}
