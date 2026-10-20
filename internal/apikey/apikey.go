// Package apikey issues and verifies merchant secret API keys (sk_test_..., sk_live_...).
package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

type Mode string

const (
	ModeTest Mode = "test"
	ModeLive Mode = "live"
)

func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case ModeTest, ModeLive:
		return Mode(s), nil
	}
	return "", fmt.Errorf("invalid mode %q: want test or live", s)
}

// displayPrefixLen keeps "sk_test_" plus 4 random characters, enough to tell keys apart in a list.
const displayPrefixLen = 12

// Key is a freshly generated secret. Secret is shown to the merchant once; only Hash is stored.
type Key struct {
	Mode   Mode
	Secret string
	Prefix string
	Hash   []byte
}

func Generate(mode Mode) (Key, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return Key{}, err
	}
	secret := "sk_" + string(mode) + "_" + hex.EncodeToString(b)
	return Key{Mode: mode, Secret: secret, Prefix: secret[:displayPrefixLen], Hash: Hash(secret)}, nil
}

// Hash is a plain SHA-256: keys carry 192 random bits, so a slow password hash adds nothing.
func Hash(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// LooksLikeKey rejects obviously malformed input before it reaches the database.
func LooksLikeKey(s string) bool {
	return strings.HasPrefix(s, "sk_test_") || strings.HasPrefix(s, "sk_live_")
}
