// Package paymentintent models a PaymentIntent: one attempt to collect an amount from a customer,
// moving through a fixed set of statuses.
package paymentintent

import (
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"time"
)

type Status string

const (
	StatusRequiresPaymentMethod Status = "requires_payment_method"
	StatusRequiresConfirmation  Status = "requires_confirmation"
	StatusRequiresAction        Status = "requires_action"
	StatusProcessing            Status = "processing"
	StatusRequiresCapture       Status = "requires_capture"
	StatusSucceeded             Status = "succeeded"
	StatusCanceled              Status = "canceled"
)

// transitions is the state machine: every status a PaymentIntent may move to from each status.
// succeeded and canceled are terminal. Refunds never change a PaymentIntent's status.
var transitions = map[Status][]Status{
	StatusRequiresPaymentMethod: {StatusRequiresConfirmation, StatusProcessing, StatusCanceled},
	StatusRequiresConfirmation:  {StatusRequiresPaymentMethod, StatusProcessing, StatusCanceled},
	StatusRequiresAction:        {StatusRequiresPaymentMethod, StatusProcessing, StatusCanceled},
	StatusProcessing:            {StatusRequiresPaymentMethod, StatusRequiresAction, StatusRequiresCapture, StatusSucceeded},
	StatusRequiresCapture:       {StatusSucceeded, StatusCanceled},
	StatusSucceeded:             {},
	StatusCanceled:              {},
}

var ErrInvalidTransition = errors.New("invalid status transition")

// CheckTransition returns an error wrapping ErrInvalidTransition unless from -> to is allowed.
func CheckTransition(from, to Status) error {
	if !slices.Contains(transitions[from], to) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
	}
	return nil
}

type CaptureMethod string

const (
	CaptureAutomatic CaptureMethod = "automatic"
	CaptureManual    CaptureMethod = "manual"
)

type PaymentIntent struct {
	ID            string
	MerchantID    int64
	Livemode      bool
	Amount        int64 // in the currency's minor unit: cents for usd, whole yen for jpy
	Currency      string
	Status        Status
	CaptureMethod CaptureMethod
	Description   *string
	PaymentMethod *string
	Created       time.Time
}

type CreateParams struct {
	Amount        int64
	Currency      string
	CaptureMethod CaptureMethod // empty means automatic
	Description   *string
	PaymentMethod *string
}

// ValidationError names the request parameter that is wrong, so the API can report it.
type ValidationError struct {
	Param   string
	Code    string
	Message string
}

func (e *ValidationError) Error() string { return e.Param + ": " + e.Message }

// MaxAmount is the largest amount accepted in any currency (99,999,999 minor units, as Stripe).
const MaxAmount = 99_999_999

// minAmounts lists the supported currencies and the smallest charge each allows, in minor units.
var minAmounts = map[string]int64{
	"usd": 50,
	"eur": 50,
	"gbp": 30,
	"jpy": 50,
	"vnd": 10_000,
}

// Validate checks the parameters and fills in defaults.
func (p *CreateParams) Validate() error {
	if p.CaptureMethod == "" {
		p.CaptureMethod = CaptureAutomatic
	}
	if p.CaptureMethod != CaptureAutomatic && p.CaptureMethod != CaptureManual {
		return &ValidationError{"capture_method", "parameter_invalid_string",
			fmt.Sprintf("Invalid capture_method: must be one of automatic or manual, got %q.", p.CaptureMethod)}
	}
	minAmount, ok := minAmounts[p.Currency]
	if !ok {
		return &ValidationError{"currency", "currency_unsupported",
			fmt.Sprintf("Invalid currency: %q. Supported currencies are eur, gbp, jpy, usd, vnd.", p.Currency)}
	}
	if p.Amount < minAmount {
		return &ValidationError{"amount", "amount_too_small",
			fmt.Sprintf("Amount must be at least %d %s (in the currency's smallest unit).", minAmount, p.Currency)}
	}
	if p.Amount > MaxAmount {
		return &ValidationError{"amount", "amount_too_large",
			fmt.Sprintf("Amount must be no more than %d.", MaxAmount)}
	}
	if p.Description != nil && len(*p.Description) > 1000 {
		return &ValidationError{"description", "parameter_invalid_string", "Description must be at most 1000 characters."}
	}
	return nil
}

// InitialStatus is where a new PaymentIntent starts: it needs a payment method before it can be confirmed.
func (p CreateParams) InitialStatus() Status {
	if p.PaymentMethod != nil {
		return StatusRequiresConfirmation
	}
	return StatusRequiresPaymentMethod
}

// NewID returns a public ID like "pi_" followed by 26 random base32 characters (128 bits).
func NewID() string {
	return "pi_" + rand.Text()
}
