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
	Metadata      map[string]string
	Created       time.Time

	AmountCapturable   int64 // authorized and not yet captured
	AmountReceived     int64 // captured
	Attempts           int
	AuthorizationID    *string
	LastPaymentError   *PaymentError
	CanceledAt         *time.Time
	CancellationReason *string
}

// PaymentError explains why the last confirmation failed, e.g. a declined card.
type PaymentError struct {
	Type          string `json:"type"`
	Code          string `json:"code"`
	DeclineCode   string `json:"decline_code,omitempty"`
	Message       string `json:"message"`
	PaymentMethod string `json:"payment_method,omitempty"`
}

// networkReference is what the card network deduplicates on: one per confirmation attempt, so a
// retry of the same attempt never places a second hold, while a new card after a decline is a new attempt.
func (pi PaymentIntent) networkReference() string {
	return fmt.Sprintf("%s:%d", pi.ID, pi.Attempts)
}

type CreateParams struct {
	Amount        int64
	Currency      string
	CaptureMethod CaptureMethod // empty means automatic
	Description   *string
	PaymentMethod *string
	Metadata      MetadataUpdate
}

// UpdateParams changes the fields a merchant may edit at any status. Nil means unchanged.
type UpdateParams struct {
	Description *string
	Metadata    *MetadataUpdate
}

// StateError means the action is not allowed in the PaymentIntent's current status.
type StateError struct {
	Action string
	Status Status
}

func (e *StateError) Error() string {
	return fmt.Sprintf("You cannot %s this PaymentIntent because it has a status of %s.", e.Action, e.Status)
}

// DeclineError means the card network declined the card. The PaymentIntent is back in
// requires_payment_method and can be confirmed again with another card.
type DeclineError struct {
	Intent PaymentIntent
}

func (e *DeclineError) Error() string { return e.Intent.LastPaymentError.Message }

// ErrNetworkUnavailable means the card network did not answer a capture or void. Both are
// idempotent on the network side, so the client can simply retry.
var ErrNetworkUnavailable = errors.New("the card network did not respond; retry the request")

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
	return validateDescription(p.Description)
}

func validateDescription(d *string) error {
	if d != nil && len(*d) > 1000 {
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
