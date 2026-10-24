// Command api runs the payments HTTP API.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongthanhson/stripe-clone/internal/apikey"
	"github.com/truongthanhson/stripe-clone/internal/cardnetwork"
	"github.com/truongthanhson/stripe-clone/internal/checkout"
	"github.com/truongthanhson/stripe-clone/internal/customer"
	"github.com/truongthanhson/stripe-clone/internal/idempotency"
	"github.com/truongthanhson/stripe-clone/internal/ledger"
	"github.com/truongthanhson/stripe-clone/internal/paymentintent"
	"github.com/truongthanhson/stripe-clone/internal/refund"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if err := run(logger); err != nil {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

// networkTimeout bounds each card network call. It stays well under WriteTimeout, so the
// client always gets an answer (possibly "processing") instead of a dropped connection.
const networkTimeout = 5 * time.Second

func run(logger *slog.Logger) error {
	addr := ":" + getenv("PORT", "8080")

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	// The in-process fake is the only card network until a processor adapter exists; live mode
	// refuses to confirm, so it only ever moves test money.
	network := cardnetwork.NewFake()
	idem := idempotency.NewStore(pool)
	go pruneIdempotencyKeys(logger, idem)
	intents := paymentintent.NewService(paymentintent.NewStore(pool), network, networkTimeout, logger)

	srv := &http.Server{
		Addr: addr,
		Handler: withRequestLogging(logger, newRouter(app{
			db:        pool,
			keys:      apikey.NewStore(pool),
			intents:   intents,
			refunds:   refund.NewService(pool, network, networkTimeout, logger),
			customers: customer.NewStore(pool),
			balances:  ledger.NewReader(pool),
			idem:      idem,
			checkout:  checkout.NewService(pool, intents),
			publicURL: getenv("PUBLIC_URL", "http://localhost"+addr),
		})),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// pruneIdempotencyKeys deletes expired keys hourly so the table stays small.
func pruneIdempotencyKeys(logger *slog.Logger, store *idempotency.Store) {
	for range time.Tick(time.Hour) {
		n, err := store.Prune(context.Background())
		if err != nil {
			logger.Error("prune idempotency keys", "error", err)
			continue
		}
		logger.Info("pruned idempotency keys", "deleted", n)
	}
}

type pinger interface {
	Ping(ctx context.Context) error
}

// app holds the dependencies the handlers need; tests fill it with fakes.
type app struct {
	db        pinger
	keys      keyLookup
	intents   intentStore
	balances  balanceStore
	refunds   refundStore
	customers customerStore
	idem      idempotencyStore // nil disables Idempotency-Key handling
	checkout  checkoutStore
	publicURL string // where browsers reach this server, for hosted page links
}

// route is one endpoint. Keeping the table as data lets a test compare it with openapi.yaml.
type route struct {
	pattern string // "METHOD /path", as http.ServeMux takes it
	handler http.Handler
}

func routes(a app) []route {
	spec := loadSpec()
	// Order matters: authenticate, pick the API version, validate against the spec, then claim the
	// idempotency key, so a malformed request never stores a response under its key.
	authed := func(h http.HandlerFunc) http.Handler {
		return requireAPIKey(a.keys, withAPIVersion(withRequestValidation(spec, h)))
	}
	authedPost := func(h http.HandlerFunc) http.Handler {
		return requireAPIKey(a.keys, withAPIVersion(withRequestValidation(spec, withIdempotency(a.idem, h))))
	}
	return []route{
		{"GET /healthz", handleHealthz(a.db)},
		{"GET /openapi.yaml", http.HandlerFunc(handleSpec)},
		{"GET /v1/account", authed(handleAccount)},
		{"POST /v1/payment_intents", authedPost(handleCreateIntent(a.intents))},
		{"GET /v1/payment_intents", authed(handleListIntents(a.intents))},
		{"GET /v1/payment_intents/{id}", authed(handleGetIntent(a.intents))},
		{"POST /v1/payment_intents/{id}", authedPost(handleUpdateIntent(a.intents))},
		{"POST /v1/payment_intents/{id}/confirm", authedPost(handleConfirmIntent(a.intents))},
		{"POST /v1/payment_intents/{id}/capture", authedPost(handleCaptureIntent(a.intents))},
		{"POST /v1/payment_intents/{id}/cancel", authedPost(handleCancelIntent(a.intents))},
		{"POST /v1/customers", authedPost(handleCreateCustomer(a.customers))},
		{"GET /v1/customers", authed(handleListCustomers(a.customers))},
		{"GET /v1/customers/{id}", authed(handleGetCustomer(a.customers))},
		{"POST /v1/customers/{id}", authedPost(handleUpdateCustomer(a.customers))},
		{"DELETE /v1/customers/{id}", authed(handleDeleteCustomer(a.customers))},
		{"POST /v1/refunds", authedPost(handleCreateRefund(a.refunds))},
		{"GET /v1/refunds", authed(handleListRefunds(a.refunds))},
		{"GET /v1/refunds/{id}", authed(handleGetRefund(a.refunds))},
		{"GET /v1/balance", authed(handleGetBalance(a.balances))},
		{"GET /v1/balance_transactions", authed(handleListBalanceTransactions(a.balances))},
		{"GET /v1/balance_transactions/{id}", authed(handleGetBalanceTransaction(a.balances))},
		{"POST /v1/checkout/sessions", authedPost(handleCreateCheckoutSession(a.checkout, a.publicURL))},
		{"GET /v1/checkout/sessions", authed(handleListCheckoutSessions(a.checkout, a.publicURL))},
		{"GET /v1/checkout/sessions/{id}", authed(handleGetCheckoutSession(a.checkout, a.publicURL))},
		{"GET /v1/checkout/sessions/{id}/line_items", authed(handleListCheckoutLineItems(a.checkout))},
		{"POST /v1/checkout/sessions/{id}/expire", authedPost(handleExpireCheckoutSession(a.checkout, a.publicURL))},
	}
}

// pageRoutes are the HTML pages people open in a browser. They are not part of the API, so they are
// not in openapi.yaml and take no API key.
func pageRoutes(a app) []route {
	return []route{
		{"GET /static/", handleStatic()},
		{"GET /c/pay/{id}", withPageHeaders(handleCheckoutPage(a.checkout))},
		{"POST /c/pay/{id}", withPageHeaders(handleCheckoutPay(a.checkout))},
	}
}

func newRouter(a app) http.Handler {
	mux := http.NewServeMux()
	for _, rt := range append(routes(a), pageRoutes(a)...) {
		mux.Handle(rt.pattern, rt.handler)
	}
	return mux
}

func handleAccount(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"object":   "account",
		"id":       p.MerchantID,
		"livemode": p.Livemode(),
	})
}

func handleHealthz(db pinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		w.Header().Set("Content-Type", "application/json")
		if err := db.Ping(ctx); err != nil {
			loggerFrom(r.Context()).Warn("database ping failed", "error", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "unavailable", "database": "down"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "database": "up"})
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
