// Package webhook sends events to the URLs merchants register, signed so a receiver can tell they
// came from us, and retried until the receiver acknowledges them.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SignatureHeader carries "t=<unix seconds>,v1=<hex HMAC-SHA256>", as Stripe's does.
const SignatureHeader = "Stripe-Signature"

// DefaultTolerance is how old a signature a receiver should accept. The timestamp is inside the
// signed content, so an attacker who captured a delivery cannot replay it after this window.
const DefaultTolerance = 5 * time.Minute

// Sign returns the signature header value for payload sent at t: the HMAC of "t.payload".
func Sign(secret string, t time.Time, payload []byte) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac(secret, ts, payload))
}

func mac(secret, ts string, payload []byte) []byte {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(ts))
	h.Write([]byte("."))
	h.Write(payload)
	return h.Sum(nil)
}

var (
	ErrNoSignature  = errors.New("no valid v1 signature for this payload")
	ErrTooOld       = errors.New("signature timestamp outside the tolerance")
	ErrBadSignature = errors.New("malformed signature header")
)

// Verify checks a signature header the way a receiver should: the HMAC must match (compared in
// constant time) and the timestamp must be within tolerance of now.
func Verify(payload []byte, header, secret string, tolerance time.Duration, now time.Time) error {
	var ts string
	var sigs [][]byte
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return ErrBadSignature
		}
		switch k {
		case "t":
			ts = v
		case "v1":
			if sig, err := hex.DecodeString(v); err == nil {
				sigs = append(sigs, sig)
			}
		}
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return ErrBadSignature
	}
	if d := now.Sub(time.Unix(sec, 0)); d > tolerance || d < -tolerance {
		return fmt.Errorf("%w: %s", ErrTooOld, d.Round(time.Second))
	}
	want := mac(secret, ts, payload)
	for _, sig := range sigs {
		if hmac.Equal(sig, want) {
			return nil
		}
	}
	return ErrNoSignature
}
