package webhook

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSignVerify(t *testing.T) {
	now := time.Unix(1_792_800_000, 0)
	payload := []byte(`{"id":"evt_1","type":"payment_intent.succeeded"}`)
	header := Sign("whsec_test", now, payload)
	if !strings.HasPrefix(header, "t=1792800000,v1=") {
		t.Fatalf("header = %q", header)
	}

	tests := []struct {
		name    string
		payload []byte
		header  string
		secret  string
		now     time.Time
		want    error
	}{
		{"valid", payload, header, "whsec_test", now, nil},
		{"valid a bit later", payload, header, "whsec_test", now.Add(4 * time.Minute), nil},
		{"one of several signatures matches", payload, header + ",v1=00ff", "whsec_test", now, nil},
		{"tampered payload", []byte(`{"id":"evt_2"}`), header, "whsec_test", now, ErrNoSignature},
		{"wrong secret", payload, header, "whsec_other", now, ErrNoSignature},
		{"replayed too late", payload, header, "whsec_test", now.Add(6 * time.Minute), ErrTooOld},
		{"timestamp changed", payload, strings.Replace(header, "t=1792800000", "t=1792800001", 1), "whsec_test", now, ErrNoSignature},
		{"no timestamp", payload, "v1=abcd", "whsec_test", now, ErrBadSignature},
		{"garbage", payload, "nonsense", "whsec_test", now, ErrBadSignature},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Verify(tt.payload, tt.header, tt.secret, DefaultTolerance, tt.now)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Verify = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestRetryAfter(t *testing.T) {
	if retryAfter(1) != time.Minute || retryAfter(7) != 12*time.Hour || retryAfter(50) != 12*time.Hour {
		t.Fatal("backoff schedule changed")
	}
}
