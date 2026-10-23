package paymentintent

import (
	"errors"
	"regexp"
	"testing"
)

var allStatuses = []Status{
	StatusRequiresPaymentMethod, StatusRequiresConfirmation, StatusRequiresAction,
	StatusProcessing, StatusRequiresCapture, StatusSucceeded, StatusCanceled,
}

// Adding a status without deciding where it can go must fail here, not in production.
func TestEveryStatusHasTransitions(t *testing.T) {
	for _, s := range allStatuses {
		if _, ok := transitions[s]; !ok {
			t.Errorf("status %q has no entry in transitions", s)
		}
	}
	if len(transitions) != len(allStatuses) {
		t.Errorf("transitions has %d entries, want %d", len(transitions), len(allStatuses))
	}
}

func TestCheckTransition(t *testing.T) {
	allowed := []struct{ from, to Status }{
		{StatusRequiresPaymentMethod, StatusRequiresConfirmation},
		{StatusRequiresConfirmation, StatusProcessing},
		{StatusProcessing, StatusSucceeded},
		{StatusProcessing, StatusRequiresCapture},
		{StatusProcessing, StatusRequiresPaymentMethod}, // declined: try another card
		{StatusRequiresCapture, StatusSucceeded},
		{StatusRequiresCapture, StatusCanceled},
	}
	for _, tt := range allowed {
		if err := CheckTransition(tt.from, tt.to); err != nil {
			t.Errorf("%s -> %s: %v, want allowed", tt.from, tt.to, err)
		}
	}

	forbidden := []struct{ from, to Status }{
		{StatusCanceled, StatusSucceeded},
		{StatusSucceeded, StatusCanceled},
		{StatusSucceeded, StatusRequiresPaymentMethod},
		{StatusRequiresPaymentMethod, StatusSucceeded}, // never succeed without processing
		{StatusRequiresConfirmation, StatusRequiresCapture},
		{StatusProcessing, StatusCanceled}, // the network call is in flight
	}
	for _, tt := range forbidden {
		if err := CheckTransition(tt.from, tt.to); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s -> %s: err = %v, want ErrInvalidTransition", tt.from, tt.to, err)
		}
	}

	for _, terminal := range []Status{StatusSucceeded, StatusCanceled} {
		for _, to := range allStatuses {
			if CheckTransition(terminal, to) == nil {
				t.Errorf("terminal status %s can move to %s", terminal, to)
			}
		}
	}
}

func ptr[T any](v T) *T { return &v }

func TestCreateParamsValidate(t *testing.T) {
	tests := []struct {
		name      string
		params    CreateParams
		wantParam string
		wantCode  string
	}{
		{"ok", CreateParams{Amount: 2000, Currency: "usd"}, "", ""},
		{"minimum usd", CreateParams{Amount: 50, Currency: "usd"}, "", ""},
		{"below minimum", CreateParams{Amount: 49, Currency: "usd"}, "amount", "amount_too_small"},
		{"zero", CreateParams{Amount: 0, Currency: "usd"}, "amount", "amount_too_small"},
		{"negative", CreateParams{Amount: -100, Currency: "usd"}, "amount", "amount_too_small"},
		{"too large", CreateParams{Amount: MaxAmount + 1, Currency: "usd"}, "amount", "amount_too_large"},
		{"zero-decimal currency uses whole units", CreateParams{Amount: 50, Currency: "jpy"}, "", ""},
		{"unsupported currency", CreateParams{Amount: 2000, Currency: "xyz"}, "currency", "currency_unsupported"},
		{"bad capture method", CreateParams{Amount: 2000, Currency: "usd", CaptureMethod: "later"}, "capture_method", "parameter_invalid_string"},
		{"long description", CreateParams{Amount: 2000, Currency: "usd", Description: ptr(string(make([]byte, 1001)))}, "description", "parameter_invalid_string"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.params.Validate()
			if tt.wantParam == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Param != tt.wantParam || ve.Code != tt.wantCode {
				t.Fatalf("Validate() = %v, want param %q code %q", err, tt.wantParam, tt.wantCode)
			}
		})
	}
}

func TestValidateDefaultsCaptureMethod(t *testing.T) {
	p := CreateParams{Amount: 2000, Currency: "usd"}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if p.CaptureMethod != CaptureAutomatic {
		t.Fatalf("capture method = %q, want automatic", p.CaptureMethod)
	}
}

func TestInitialStatus(t *testing.T) {
	if s := (CreateParams{}).InitialStatus(); s != StatusRequiresPaymentMethod {
		t.Errorf("without payment method: %s", s)
	}
	if s := (CreateParams{PaymentMethod: ptr("pm_card_visa")}).InitialStatus(); s != StatusRequiresConfirmation {
		t.Errorf("with payment method: %s", s)
	}
}

func TestNewID(t *testing.T) {
	id := NewID()
	if !regexp.MustCompile(`^pi_[A-Z2-7]{26}$`).MatchString(id) {
		t.Fatalf("id %q has the wrong format", id)
	}
	if NewID() == id {
		t.Fatal("two ids are identical")
	}
}
