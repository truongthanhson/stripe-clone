package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/truongthanhson/stripe-clone/internal/apikey"
	"github.com/truongthanhson/stripe-clone/internal/customer"
	"github.com/truongthanhson/stripe-clone/internal/dashboard"
	"github.com/truongthanhson/stripe-clone/internal/idempotency"
	"github.com/truongthanhson/stripe-clone/internal/ledger"
	"github.com/truongthanhson/stripe-clone/internal/listing"
	"github.com/truongthanhson/stripe-clone/internal/paymentintent"
	"github.com/truongthanhson/stripe-clone/internal/refund"
)

const dashboardPageSize = 25

func badgeClass(status any) string {
	switch fmt.Sprint(status) {
	case "succeeded", "complete":
		return "ok"
	case "processing", "pending", "requires_capture":
		return "wait"
	case "failed":
		return "bad"
	}
	return "neutral"
}

// formatMajor writes an amount the way a person types it into a form: 1250 usd is "12.50".
func formatMajor(amount int64, currency string) string {
	if zeroDecimal[currency] {
		return strconv.FormatInt(amount, 10)
	}
	return fmt.Sprintf("%d.%02d", amount/100, amount%100)
}

// parseMajor reads a typed amount such as "12.50" into minor units without going through a float,
// which cannot hold most cent values exactly.
func parseMajor(s, currency string) (int64, error) {
	s = strings.TrimSpace(s)
	whole, frac, hasFrac := strings.Cut(s, ".")
	decimals := 2
	if zeroDecimal[currency] {
		decimals = 0
	}
	invalid := fmt.Errorf("Enter an amount like %s.", formatMajor(1050, currency))
	if whole == "" || !digits(whole) || len(whole) > 9 || (hasFrac && (frac == "" || !digits(frac) || len(frac) > decimals)) {
		return 0, invalid
	}
	n, _ := strconv.ParseInt(whole, 10, 64)
	for i := 0; i < decimals; i++ {
		n *= 10
	}
	if hasFrac {
		f, _ := strconv.ParseInt(frac, 10, 64)
		if len(frac) == 1 {
			f *= 10
		}
		n += f
	}
	if n <= 0 {
		return 0, errors.New("Enter an amount greater than zero.")
	}
	return n, nil
}

func digits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func notFoundPage(w http.ResponseWriter, r *http.Request, what string) {
	sess := sessionFrom(r.Context())
	renderDashboard(w, r, http.StatusNotFound, "error.html", dashPage{Title: "Not found", Session: &sess,
		Error: "No such " + what + " in " + modeLabel(sess.Livemode) + " data."})
}

func modeLabel(live bool) string {
	if live {
		return "live"
	}
	return "test"
}

// dashboardRoutes are the merchant dashboard's pages. Like the checkout page they are HTML, not API,
// so they are not in openapi.yaml; a signed-in session replaces the API key.
func dashboardRoutes(a app) []route {
	c := cookies{secure: strings.HasPrefix(a.publicURL, "https://")}
	page := func(h http.HandlerFunc) http.Handler {
		return withDashboardHeaders(requireSession(a.dashboard, h))
	}
	return []route{
		{"GET /dashboard", http.RedirectHandler("/dashboard/", http.StatusMovedPermanently)},
		{"GET /dashboard/login", withDashboardHeaders(handleLoginPage(a.dashboard, c))},
		{"POST /dashboard/login", withDashboardHeaders(handleLogin(a.dashboard, c))},
		{"POST /dashboard/logout", page(handleLogout(a.dashboard, c))},
		{"POST /dashboard/mode", page(handleSwitchMode(a.dashboard, c))},
		{"GET /dashboard/{$}", page(handleDashboardHome(a))},
		{"GET /dashboard/payments", page(handleDashboardPayments(a))},
		{"GET /dashboard/payments/{id}", page(handleDashboardPayment(a))},
		{"POST /dashboard/payments/{id}/capture", page(handleDashboardCapture(a, c))},
		{"POST /dashboard/payments/{id}/cancel", page(handleDashboardCancel(a, c))},
		{"POST /dashboard/payments/{id}/refunds", page(handleDashboardRefund(a, c))},
		{"GET /dashboard/customers", page(handleDashboardCustomers(a))},
		{"GET /dashboard/customers/{id}", page(handleDashboardCustomer(a))},
		{"GET /dashboard/balance", page(handleDashboardBalance(a))},
		{"GET /dashboard/apikeys", page(handleDashboardKeys(a))},
		{"POST /dashboard/apikeys", page(handleDashboardCreateKey(a))},
		{"POST /dashboard/apikeys/{id}/revoke", page(handleDashboardRevokeKey(a, c))},
	}
}

type homeData struct {
	Balances []ledger.Balance
	Payments pageData[paymentintent.PaymentIntent]
}

func handleDashboardHome(a app) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFrom(r.Context())
		balances, err := a.balances.Balances(r.Context(), sess.MerchantID, sess.Livemode)
		if err != nil {
			dashboardInternalError(w, r, "dashboard home", err)
			return
		}
		payments, _, err := a.intents.List(r.Context(), sess.MerchantID, sess.Livemode, "", listing.Params{Limit: 10})
		if err != nil {
			dashboardInternalError(w, r, "dashboard home", err)
			return
		}
		renderDashboard(w, r, http.StatusOK, "home.html", dashPage{Title: "Home", Nav: "home", Session: &sess,
			Data: homeData{Balances: balances, Payments: pageData[paymentintent.PaymentIntent]{Items: payments}}})
	}
}

// pageData is one page of a list; Next is the starting_after for the following page, empty on the last.
type pageData[T any] struct {
	Items []T
	Next  string
}

func nextCursor[T any](items []T, hasMore bool, id func(T) string) string {
	if !hasMore || len(items) == 0 {
		return ""
	}
	return id(items[len(items)-1])
}

func dashboardListParams(r *http.Request) listing.Params {
	return listing.Params{Limit: dashboardPageSize, StartingAfter: r.URL.Query().Get("starting_after")}
}

func handleDashboardPayments(a app) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFrom(r.Context())
		items, hasMore, err := a.intents.List(r.Context(), sess.MerchantID, sess.Livemode, "", dashboardListParams(r))
		if errors.Is(err, paymentintent.ErrNotFound) {
			notFoundPage(w, r, "payment")
			return
		}
		if err != nil {
			dashboardInternalError(w, r, "dashboard payments", err)
			return
		}
		renderDashboard(w, r, http.StatusOK, "payments.html", dashPage{Title: "Payments", Nav: "payments", Session: &sess,
			Data: pageData[paymentintent.PaymentIntent]{Items: items, Next: nextCursor(items, hasMore, func(pi paymentintent.PaymentIntent) string { return pi.ID })}})
	}
}

type paymentData struct {
	Intent      paymentintent.PaymentIntent
	Refunds     []refund.Refund
	Refunded    int64 // succeeded refunds
	Refundable  int64 // received, less refunds that succeeded or may still succeed
	Cancelable  bool
	RefundToken string // makes a double-submitted refund form refund once
}

func loadPayment(ctx context.Context, a app, id string) (paymentData, error) {
	sess := sessionFrom(ctx)
	pi, err := a.intents.Get(ctx, sess.MerchantID, sess.Livemode, id)
	if err != nil {
		return paymentData{}, err
	}
	refunds, _, err := a.refunds.List(ctx, sess.MerchantID, sess.Livemode, pi.ID, listing.Params{Limit: 100})
	if err != nil {
		return paymentData{}, err
	}
	d := paymentData{Intent: pi, Refunds: refunds, Refundable: pi.AmountReceived, RefundToken: dashboard.NewToken()}
	for _, re := range refunds {
		switch re.Status {
		case refund.StatusSucceeded:
			d.Refunded += re.Amount
			d.Refundable -= re.Amount
		case refund.StatusPending:
			d.Refundable -= re.Amount
		}
	}
	d.Cancelable = paymentintent.CheckTransition(pi.Status, paymentintent.StatusCanceled) == nil
	return d, nil
}

func handleDashboardPayment(a app) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		renderPayment(w, r, a, http.StatusOK, "")
	}
}

// renderPayment shows the payment page, with msg as an error when an action on it failed.
func renderPayment(w http.ResponseWriter, r *http.Request, a app, status int, msg string) {
	sess := sessionFrom(r.Context())
	d, err := loadPayment(r.Context(), a, r.PathValue("id"))
	if errors.Is(err, paymentintent.ErrNotFound) {
		notFoundPage(w, r, "payment")
		return
	}
	if err != nil {
		dashboardInternalError(w, r, "dashboard payment", err)
		return
	}
	renderDashboard(w, r, status, "payment.html", dashPage{Title: "Payment", Nav: "payments", Session: &sess, Error: msg, Data: d})
}

// actionError turns what a payment action returned into a message for the merchant. ok is false
// for errors that are not the merchant's to fix, which get the generic error page.
func actionError(err error) (int, string, bool) {
	var (
		ve  *paymentintent.ValidationError
		se  *paymentintent.StateError
		rve *refund.ValidationError
	)
	switch {
	case errors.As(err, &ve):
		return http.StatusUnprocessableEntity, ve.Message, true
	case errors.As(err, &rve):
		return http.StatusUnprocessableEntity, rve.Message, true
	case errors.As(err, &se):
		return http.StatusConflict, se.Error(), true
	case errors.Is(err, paymentintent.ErrNetworkUnavailable):
		return http.StatusBadGateway, "The card network did not answer, so nothing changed. Try again.", true
	}
	return 0, "", false
}

func paymentPath(id string) string { return "/dashboard/payments/" + id }

// amountFromForm reads an optional amount field; empty means the whole amount.
func amountFromForm(r *http.Request, a app, field string) (*int64, string, error) {
	raw := r.PostForm.Get(field)
	if strings.TrimSpace(raw) == "" {
		return nil, "", nil
	}
	sess := sessionFrom(r.Context())
	pi, err := a.intents.Get(r.Context(), sess.MerchantID, sess.Livemode, r.PathValue("id"))
	if err != nil {
		return nil, "", err
	}
	n, perr := parseMajor(raw, pi.Currency)
	if perr != nil {
		return nil, perr.Error(), nil
	}
	return &n, "", nil
}

func handleDashboardCapture(a app, c cookies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFrom(r.Context())
		amount, msg, err := amountFromForm(r, a, "amount")
		if msg != "" {
			renderPayment(w, r, a, http.StatusUnprocessableEntity, msg)
			return
		}
		if err == nil {
			_, err = a.intents.Capture(r.Context(), sess.MerchantID, sess.Livemode, r.PathValue("id"), amount)
		}
		finishAction(w, r, a, c, err, "captured")
	}
}

func handleDashboardCancel(a app, c cookies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFrom(r.Context())
		reason := r.PostForm.Get("reason")
		var reasonPtr *string
		if reason != "" {
			reasonPtr = &reason
		}
		_, err := a.intents.Cancel(r.Context(), sess.MerchantID, sess.Livemode, r.PathValue("id"), reasonPtr)
		finishAction(w, r, a, c, err, "canceled")
	}
}

func finishAction(w http.ResponseWriter, r *http.Request, a app, c cookies, err error, flash string) {
	if errors.Is(err, paymentintent.ErrNotFound) {
		notFoundPage(w, r, "payment")
		return
	}
	if err != nil {
		status, msg, ok := actionError(err)
		if !ok {
			dashboardInternalError(w, r, "dashboard payment action", err)
			return
		}
		renderPayment(w, r, a, status, msg)
		return
	}
	redirectWithFlash(w, r, c, paymentPath(r.PathValue("id")), flash)
}

// handleDashboardRefund refunds through the same Idempotency-Key store the API uses, keyed by a
// token rendered into the form: a double click or a resubmitted form refunds once.
func handleDashboardRefund(a app, c cookies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFrom(r.Context())
		id := r.PathValue("id")
		amount, msg, err := amountFromForm(r, a, "amount")
		if errors.Is(err, paymentintent.ErrNotFound) {
			notFoundPage(w, r, "payment")
			return
		}
		if err != nil {
			dashboardInternalError(w, r, "dashboard refund", err)
			return
		}
		if msg != "" {
			renderPayment(w, r, a, http.StatusUnprocessableEntity, msg)
			return
		}
		params := refund.CreateParams{PaymentIntentID: id, Amount: amount}
		if reason := r.PostForm.Get("reason"); reason != "" {
			params.Reason = &reason
		}

		token := r.PostForm.Get("refund_token")
		if a.idem != nil && token != "" {
			sc := idempotency.Scope{MerchantID: sess.MerchantID, Livemode: sess.Livemode, Key: "dashboard-refund-" + token}
			stored, err := a.idem.Begin(r.Context(), sc, idempotency.Fingerprint("POST", r.URL.Path, []byte(r.PostForm.Encode())))
			switch {
			case stored != nil, errors.Is(err, idempotency.ErrInProgress):
				http.Redirect(w, r, paymentPath(id), http.StatusSeeOther) // the first submit is doing or did the refund
				return
			case errors.Is(err, idempotency.ErrMismatch):
				renderPayment(w, r, a, http.StatusConflict, "This refund form was already used. Check the refunds below before refunding again.")
				return
			case err != nil:
				dashboardInternalError(w, r, "dashboard refund", err)
				return
			}
			re, refundErr := a.refunds.Create(r.Context(), sess.MerchantID, sess.Livemode, params)
			ctx := context.WithoutCancel(r.Context())
			if refundErr != nil {
				err = a.idem.Release(ctx, sc)
			} else {
				err = a.idem.Complete(ctx, sc, idempotency.Response{Status: http.StatusSeeOther, Body: []byte(re.ID)})
			}
			if err != nil {
				loggerFrom(r.Context()).Error("dashboard refund idempotency", "error", err)
			}
			finishRefund(w, r, a, c, re, refundErr)
			return
		}
		re, err := a.refunds.Create(r.Context(), sess.MerchantID, sess.Livemode, params)
		finishRefund(w, r, a, c, re, err)
	}
}

func finishRefund(w http.ResponseWriter, r *http.Request, a app, c cookies, re refund.Refund, err error) {
	if err != nil {
		finishAction(w, r, a, c, err, "")
		return
	}
	flash := "refunded"
	switch re.Status {
	case refund.StatusPending:
		flash = "refund_pending"
	case refund.StatusFailed:
		flash = "refund_failed"
	}
	redirectWithFlash(w, r, c, paymentPath(r.PathValue("id")), flash)
}

func handleDashboardCustomers(a app) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFrom(r.Context())
		items, hasMore, err := a.customers.List(r.Context(), sess.MerchantID, sess.Livemode, "", dashboardListParams(r))
		if errors.Is(err, customer.ErrNotFound) {
			notFoundPage(w, r, "customer")
			return
		}
		if err != nil {
			dashboardInternalError(w, r, "dashboard customers", err)
			return
		}
		renderDashboard(w, r, http.StatusOK, "customers.html", dashPage{Title: "Customers", Nav: "customers", Session: &sess,
			Data: pageData[customer.Customer]{Items: items, Next: nextCursor(items, hasMore, func(c customer.Customer) string { return c.ID })}})
	}
}

type customerData struct {
	Customer customer.Customer
	Payments pageData[paymentintent.PaymentIntent]
}

func handleDashboardCustomer(a app) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFrom(r.Context())
		cus, err := a.customers.Get(r.Context(), sess.MerchantID, sess.Livemode, r.PathValue("id"))
		if errors.Is(err, customer.ErrNotFound) {
			notFoundPage(w, r, "customer")
			return
		}
		if err != nil {
			dashboardInternalError(w, r, "dashboard customer", err)
			return
		}
		items, hasMore, err := a.intents.List(r.Context(), sess.MerchantID, sess.Livemode, cus.ID, dashboardListParams(r))
		if errors.Is(err, paymentintent.ErrNotFound) {
			notFoundPage(w, r, "payment")
			return
		}
		if err != nil {
			dashboardInternalError(w, r, "dashboard customer", err)
			return
		}
		renderDashboard(w, r, http.StatusOK, "customer.html", dashPage{Title: "Customer", Nav: "customers", Session: &sess,
			Data: customerData{Customer: cus, Payments: pageData[paymentintent.PaymentIntent]{Items: items,
				Next: nextCursor(items, hasMore, func(pi paymentintent.PaymentIntent) string { return pi.ID })}}})
	}
}

type balanceData struct {
	Balances     []ledger.Balance
	Transactions pageData[ledger.BalanceTransaction]
}

func handleDashboardBalance(a app) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFrom(r.Context())
		balances, err := a.balances.Balances(r.Context(), sess.MerchantID, sess.Livemode)
		if err != nil {
			dashboardInternalError(w, r, "dashboard balance", err)
			return
		}
		items, hasMore, err := a.balances.ListTransactions(r.Context(), sess.MerchantID, sess.Livemode, dashboardListParams(r))
		if errors.Is(err, ledger.ErrNotFound) {
			notFoundPage(w, r, "balance transaction")
			return
		}
		if err != nil {
			dashboardInternalError(w, r, "dashboard balance", err)
			return
		}
		renderDashboard(w, r, http.StatusOK, "balance.html", dashPage{Title: "Balance", Nav: "balance", Session: &sess,
			Data: balanceData{Balances: balances, Transactions: pageData[ledger.BalanceTransaction]{Items: items,
				Next: nextCursor(items, hasMore, func(bt ledger.BalanceTransaction) string { return bt.ID })}}})
	}
}

type keysData struct {
	Keys      []apikey.Info
	NewSecret string // shown once, right after the key is created
}

func sessionMode(live bool) apikey.Mode {
	if live {
		return apikey.ModeLive
	}
	return apikey.ModeTest
}

func renderKeys(w http.ResponseWriter, r *http.Request, a app, status int, newSecret string) {
	sess := sessionFrom(r.Context())
	keys, err := a.keyAdmin.List(r.Context(), sess.MerchantID, sessionMode(sess.Livemode))
	if err != nil {
		dashboardInternalError(w, r, "dashboard api keys", err)
		return
	}
	renderDashboard(w, r, status, "apikeys.html", dashPage{Title: "API keys", Nav: "apikeys", Session: &sess,
		Data: keysData{Keys: keys, NewSecret: newSecret}})
}

func handleDashboardKeys(a app) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		renderKeys(w, r, a, http.StatusOK, "")
	}
}

// handleDashboardCreateKey answers with the page itself rather than a redirect: the secret exists
// only in this response, and the page is never cached (Cache-Control: no-store).
func handleDashboardCreateKey(a app) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFrom(r.Context())
		key, err := a.keyAdmin.Issue(r.Context(), sess.MerchantID, sessionMode(sess.Livemode))
		if err != nil {
			dashboardInternalError(w, r, "create api key", err)
			return
		}
		renderKeys(w, r, a, http.StatusOK, key.Secret)
	}
}

func handleDashboardRevokeKey(a app, c cookies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFrom(r.Context())
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err == nil {
			err = a.keyAdmin.Revoke(r.Context(), sess.MerchantID, id)
		}
		var numErr *strconv.NumError
		if errors.Is(err, apikey.ErrNotFound) || errors.As(err, &numErr) {
			notFoundPage(w, r, "active API key")
			return
		}
		if err != nil {
			dashboardInternalError(w, r, "revoke api key", err)
			return
		}
		redirectWithFlash(w, r, c, "/dashboard/apikeys", "key_revoked")
	}
}
