package main

import (
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/truongthanhson/stripe-clone/internal/checkout"
	"github.com/truongthanhson/stripe-clone/internal/paymentintent"
)

//go:embed web
var webFS embed.FS

var pageTemplates = template.Must(template.New("").Funcs(template.FuncMap{
	"money": formatMoney,
}).ParseFS(webFS, "web/templates/*.html"))

// zeroDecimal lists currencies whose smallest unit is the whole unit, so 500 jpy is ¥500, not ¥5.00.
var zeroDecimal = map[string]bool{"jpy": true, "vnd": true}

var currencySymbols = map[string]string{"usd": "$", "eur": "€", "gbp": "£", "jpy": "¥", "vnd": "₫"}

func formatMoney(amount int64, currency string) string {
	sym, ok := currencySymbols[currency]
	if !ok {
		sym = strings.ToUpper(currency) + " "
	}
	if zeroDecimal[currency] {
		return fmt.Sprintf("%s%d", sym, amount)
	}
	return fmt.Sprintf("%s%d.%02d", sym, amount/100, amount%100)
}

// withPageHeaders sets the security headers every HTML page gets. The pages run no script and load
// nothing from other origins, and no other site may frame them (a framed payment form is the classic
// clickjacking target).
func withPageHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func handleStatic() http.Handler {
	static, _ := fs.Sub(webFS, "web/static")
	return http.StripPrefix("/static/", http.FileServerFS(static))
}

type checkoutPage struct {
	Session    checkout.Session
	Email      string
	Error      string
	ErrField   string
	Processing bool
}

func renderPage(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := pageTemplates.ExecuteTemplate(w, name, data); err != nil {
		loggerFrom(r.Context()).Error("render page", "template", name, "error", err)
	}
}

func handleCheckoutPage(store checkoutStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, err := store.Open(r.Context(), r.PathValue("id"))
		if errors.Is(err, checkout.ErrNotFound) {
			renderPage(w, r, http.StatusNotFound, "checkout_missing.html", nil)
			return
		}
		if err != nil {
			loggerFrom(r.Context()).Error("checkout page", "error", err)
			renderPage(w, r, http.StatusInternalServerError, "checkout_missing.html", nil)
			return
		}
		page := checkoutPage{Session: s}
		if s.CustomerDetailsEmail != nil {
			page.Email = *s.CustomerDetailsEmail
		} else if s.CustomerEmail != nil {
			page.Email = *s.CustomerEmail
		}
		if pe := s.Intent.LastPaymentError; pe != nil {
			page.Error, page.ErrField = pe.Message, "number"
		}
		page.Processing = s.Status == checkout.StatusOpen && s.Intent.Status == paymentintent.StatusProcessing
		renderPage(w, r, http.StatusOK, "checkout.html", page)
	}
}

// handleCheckoutPay takes the submitted card form. Every outcome the session records (paid, declined,
// processing, expired) answers with a redirect, so reloading the result page never resubmits the
// card. Only mistakes in the form itself, which change nothing, are rendered directly.
func handleCheckoutPay(store checkoutStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		email := strings.TrimSpace(r.PostForm.Get("email"))
		pm, cardErr := checkout.TestCardPaymentMethod(r.PostForm.Get("number"), r.PostForm.Get("exp"), r.PostForm.Get("cvc"), time.Now())
		if cardErr == nil && !validEmail(email) {
			cardErr = &checkout.CardError{Field: "email", Message: "Enter a valid email address."}
		}
		if cardErr != nil {
			s, err := store.Open(r.Context(), id)
			if errors.Is(err, checkout.ErrNotFound) {
				renderPage(w, r, http.StatusNotFound, "checkout_missing.html", nil)
				return
			}
			if err != nil {
				loggerFrom(r.Context()).Error("checkout pay", "error", err)
				renderPage(w, r, http.StatusInternalServerError, "checkout_missing.html", nil)
				return
			}
			var ce *checkout.CardError
			errors.As(cardErr, &ce)
			renderPage(w, r, http.StatusUnprocessableEntity, "checkout.html", checkoutPage{Session: s, Email: email, Error: ce.Message, ErrField: ce.Field})
			return
		}

		s, err := store.Pay(r.Context(), id, email, pm)
		var (
			decline *paymentintent.DeclineError
			ve      *paymentintent.ValidationError
		)
		switch {
		case errors.Is(err, checkout.ErrNotFound):
			renderPage(w, r, http.StatusNotFound, "checkout_missing.html", nil)
			return
		case errors.Is(err, checkout.ErrTooManyAttempts):
			renderPage(w, r, http.StatusTooManyRequests, "checkout.html", checkoutPage{Session: s, Email: email,
				Error: "Too many attempts. Go back to the store and start the payment again.", ErrField: "number"})
			return
		case errors.As(err, &ve):
			renderPage(w, r, http.StatusUnprocessableEntity, "checkout.html", checkoutPage{Session: s, Email: email, Error: ve.Message, ErrField: "number"})
			return
		case err != nil && !errors.As(err, &decline):
			loggerFrom(r.Context()).Error("checkout pay", "checkout_session", id, "error", err)
			renderPage(w, r, http.StatusInternalServerError, "checkout_missing.html", nil)
			return
		}
		if s.Status == checkout.StatusComplete {
			http.Redirect(w, r, s.RedirectURL(), http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/c/pay/"+id, http.StatusSeeOther) // declined, processing or expired: the page says which
	}
}

func validEmail(email string) bool {
	at := strings.Index(email, "@")
	return at > 0 && at < len(email)-1 && !strings.ContainsAny(email, " \t\r\n") && len(email) <= 512
}
