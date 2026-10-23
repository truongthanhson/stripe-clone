package checkout

import (
	"strconv"
	"strings"
	"time"

	"github.com/truongthanhson/stripe-clone/internal/cardnetwork"
)

// CardError is a problem with what the buyer typed, shown next to the field. Nothing was charged.
type CardError struct {
	Field   string // number, exp or cvc
	Message string
}

func (e *CardError) Error() string { return e.Message }

// TestCardPaymentMethod turns a typed test card into its test payment method ID, checking the fields
// the way a card form does. Only test numbers are accepted: a real card number is refused before it
// goes anywhere, and the number itself is never stored or logged.
func TestCardPaymentMethod(number, exp, cvc string, now time.Time) (string, error) {
	number = strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' {
			return -1
		}
		return r
	}, number)
	if len(number) < 12 || len(number) > 19 || !digits(number) || !luhn(number) {
		return "", &CardError{"number", "Your card number is invalid."}
	}
	month, year, ok := parseExpiry(exp)
	if !ok {
		return "", &CardError{"exp", "Your card's expiration date is incomplete."}
	}
	if year < now.Year() || (year == now.Year() && month < int(now.Month())) {
		return "", &CardError{"exp", "Your card's expiration year is in the past."}
	}
	if (len(cvc) != 3 && len(cvc) != 4) || !digits(cvc) {
		return "", &CardError{"cvc", "Your card's security code is incomplete."}
	}
	pm, ok := cardnetwork.TestPaymentMethodForNumber(number)
	if !ok {
		return "", &CardError{"number", "Your card was declined. This checkout is in test mode, so use a test card such as 4242 4242 4242 4242."}
	}
	return pm, nil
}

// parseExpiry reads "MM / YY", "MM/YY" or "MM/YYYY".
func parseExpiry(exp string) (month, year int, ok bool) {
	m, y, found := strings.Cut(strings.ReplaceAll(exp, " ", ""), "/")
	if !found || len(m) < 1 || len(m) > 2 || (len(y) != 2 && len(y) != 4) || !digits(m) || !digits(y) {
		return 0, 0, false
	}
	month, _ = strconv.Atoi(m)
	year, _ = strconv.Atoi(y)
	if len(y) == 2 {
		year += 2000
	}
	return month, year, month >= 1 && month <= 12
}

func digits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// luhn is the checksum every card number carries; it catches most typos before any network call.
func luhn(number string) bool {
	sum := 0
	double := false
	for i := len(number) - 1; i >= 0; i-- {
		d := int(number[i] - '0')
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}
