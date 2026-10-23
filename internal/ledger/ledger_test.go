package ledger

import (
	"errors"
	"testing"
)

func TestFee(t *testing.T) {
	tests := []struct {
		currency string
		amount   int64
		want     int64
	}{
		{"usd", 2000, 88},   // 58 + 30
		{"usd", 50, 31},     // 1.45 rounds to 1, + 30
		{"usd", 1724, 80},   // 49.996 rounds to 50, + 30
		{"eur", 10000, 315}, // 290 + 25
		{"usd", 20, 20},     // never more than the amount
	}
	for _, tt := range tests {
		if got := Fee(tt.currency, tt.amount); got != tt.want {
			t.Errorf("Fee(%s, %d) = %d, want %d", tt.currency, tt.amount, got, tt.want)
		}
	}
}

func TestChargeBalances(t *testing.T) {
	for _, amount := range []int64{50, 99, 2000, 99_999_999} {
		tx := Charge("pi_1", 7, false, "usd", amount)
		if err := tx.Validate(); err != nil {
			t.Fatalf("charge of %d: %v", amount, err)
		}
		if tx.Entries[1].Amount != -(amount - Fee("usd", amount)) {
			t.Fatalf("merchant credit = %d", tx.Entries[1].Amount)
		}
	}
}

func TestValidate(t *testing.T) {
	a := MerchantPending(7, false, "usd")
	b := NetworkReceivable(false, "usd")
	tests := map[string]Transaction{
		"single entry":   {Currency: "usd", Entries: []Entry{{a, 0}}},
		"unbalanced":     {Currency: "usd", Entries: []Entry{{a, 100}, {b, -99}}},
		"zero entry":     {Currency: "usd", Entries: []Entry{{a, 100}, {b, -100}, {a, 0}}},
		"other currency": {Currency: "usd", Entries: []Entry{{a, 100}, {NetworkReceivable(false, "eur"), -100}}},
		"other mode":     {Currency: "usd", Entries: []Entry{{a, 100}, {NetworkReceivable(true, "usd"), -100}}},
	}
	for name, tx := range tests {
		if err := tx.Validate(); !errors.Is(err, ErrUnbalanced) {
			t.Errorf("%s: err = %v, want ErrUnbalanced", name, err)
		}
	}
}
