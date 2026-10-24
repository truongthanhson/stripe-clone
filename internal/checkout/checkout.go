// Package checkout runs hosted Checkout Sessions: a merchant creates one through the API, sends the
// buyer to its URL, and the buyer pays on a page this server renders.
//
// A session owns exactly one PaymentIntent, created with it. The PaymentIntent's compare-and-swap
// status updates decide every race (two submits, a submit against an expiry); the session's own
// status only records how the PaymentIntent ended, and is re-derived from it on every read, so a
// crash between the two updates heals itself.
package checkout

import (
	"crypto/rand"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/truongthanhson/stripe-clone/internal/metadata"
	"github.com/truongthanhson/stripe-clone/internal/paymentintent"
)

type Status string

const (
	StatusOpen     Status = "open"
	StatusComplete Status = "complete"
	StatusExpired  Status = "expired"
)

const (
	MaxLineItems = 100
	MaxQuantity  = 999_999
	// DefaultLifetime is how long a session stays payable unless expires_at says otherwise; Stripe allows
	// 30 minutes to 24 hours.
	DefaultLifetime = 24 * time.Hour
	MinLifetime     = 30 * time.Minute
	// MaxAttempts bounds card attempts per session, so a public checkout page cannot be used to test
	// stolen card numbers one after another.
	MaxAttempts = 10
)

type LineItem struct {
	Name       string `json:"name"`
	UnitAmount int64  `json:"unit_amount"`
	Quantity   int64  `json:"quantity"`
	Currency   string `json:"currency"`
}

func (li LineItem) AmountTotal() int64 { return li.UnitAmount * li.Quantity }

type Session struct {
	ID                   string
	MerchantID           int64
	MerchantName         string
	Livemode             bool
	Status               Status
	PaymentIntentID      string
	Currency             string
	AmountTotal          int64
	LineItems            []LineItem
	SuccessURL           string
	CancelURL            *string
	Customer             *string
	CustomerEmail        *string
	CustomerDetailsEmail *string
	ClientReferenceID    *string
	Metadata             map[string]string
	ExpiresAt            time.Time
	CompletedAt          *time.Time
	Created              time.Time

	// Intent is the session's PaymentIntent as of this read.
	Intent paymentintent.PaymentIntent
}

// PaymentStatus is Stripe's payment_status: whether the money has been collected.
func (s Session) PaymentStatus() string {
	if s.Status == StatusComplete {
		return "paid"
	}
	return "unpaid"
}

// RedirectURL is success_url with {CHECKOUT_SESSION_ID} replaced, as Stripe does, so the merchant's
// success page can look the session up.
func (s Session) RedirectURL() string {
	return strings.ReplaceAll(s.SuccessURL, "{CHECKOUT_SESSION_ID}", s.ID)
}

type CreateParams struct {
	LineItems         []LineItem
	SuccessURL        string
	CancelURL         *string
	Customer          *string
	CustomerEmail     *string
	ClientReferenceID *string
	Metadata          metadata.Update
	ExpiresAt         *time.Time
}

// ValidationError names the request parameter that is wrong.
type ValidationError struct {
	Param   string
	Code    string
	Message string
}

func (e *ValidationError) Error() string { return e.Param + ": " + e.Message }

// StateError means the action is not allowed in the session's current status.
type StateError struct {
	Message string
}

func (e *StateError) Error() string { return e.Message }

func (p *CreateParams) validate(now time.Time) error {
	if len(p.LineItems) == 0 {
		return &ValidationError{"line_items", "parameter_missing", "Missing required param: line_items."}
	}
	if len(p.LineItems) > MaxLineItems {
		return &ValidationError{"line_items", "parameter_invalid", fmt.Sprintf("You may pass at most %d line_items.", MaxLineItems)}
	}
	for i, li := range p.LineItems {
		param := func(field string) string { return fmt.Sprintf("line_items[%d]%s", i, field) }
		switch {
		case li.Name == "":
			return &ValidationError{param("[price_data][product_data][name]"), "parameter_missing", "Missing required param: " + param("[price_data][product_data][name]") + "."}
		case len(li.Name) > 250:
			return &ValidationError{param("[price_data][product_data][name]"), "parameter_invalid_string", "The name must be at most 250 characters."}
		case li.Currency == "":
			return &ValidationError{param("[price_data][currency]"), "parameter_missing", "Missing required param: " + param("[price_data][currency]") + "."}
		case li.Currency != p.LineItems[0].Currency:
			return &ValidationError{param("[price_data][currency]"), "parameter_invalid_string", "All line_items must use the same currency."}
		case li.UnitAmount < 0 || li.UnitAmount > paymentintent.MaxAmount:
			return &ValidationError{param("[price_data][unit_amount]"), "parameter_invalid_integer", fmt.Sprintf("unit_amount must be between 0 and %d.", paymentintent.MaxAmount)}
		case li.Quantity < 1 || li.Quantity > MaxQuantity:
			return &ValidationError{param("[quantity]"), "parameter_invalid_integer", fmt.Sprintf("quantity must be between 1 and %d.", MaxQuantity)}
		}
	}
	if err := validateURL("success_url", p.SuccessURL); err != nil {
		return err
	}
	if p.CancelURL != nil {
		if err := validateURL("cancel_url", *p.CancelURL); err != nil {
			return err
		}
	}
	if p.Customer != nil && p.CustomerEmail != nil {
		return &ValidationError{"customer_email", "parameter_invalid", "You may only specify one of customer and customer_email."}
	}
	if p.CustomerEmail != nil {
		at := strings.Index(*p.CustomerEmail, "@")
		if at < 1 || at == len(*p.CustomerEmail)-1 || strings.ContainsAny(*p.CustomerEmail, " \t\r\n") {
			return &ValidationError{"customer_email", "email_invalid", "Invalid email address: " + *p.CustomerEmail}
		}
	}
	if p.ExpiresAt != nil {
		if d := p.ExpiresAt.Sub(now); d < MinLifetime || d > DefaultLifetime {
			return &ValidationError{"expires_at", "parameter_invalid_integer", "expires_at must be between 30 minutes and 24 hours from now."}
		}
	}
	return nil
}

// validateURL accepts only absolute http(s) URLs: the buyer's browser is sent there, so a
// javascript: or data: URL would run in our page's context.
func validateURL(param, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || len(raw) > 5000 {
		return &ValidationError{param, "url_invalid", fmt.Sprintf("Invalid URL: %s must be an absolute http or https URL.", param)}
	}
	return nil
}

func (p CreateParams) amountTotal() int64 {
	var total int64
	for _, li := range p.LineItems {
		total += li.AmountTotal() // at most 100 × 99,999,999 × 999,999: fits in int64
	}
	return total
}

// newID returns "cs_test_" or "cs_live_" and 128 random bits. The ID is the only secret guarding the
// public payment page, as it is for Stripe's.
func newID(livemode bool) string {
	if livemode {
		return "cs_live_" + rand.Text()
	}
	return "cs_test_" + rand.Text()
}
