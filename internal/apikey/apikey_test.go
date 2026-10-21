package apikey

import (
	"bytes"
	"regexp"
	"testing"
)

func TestGenerate(t *testing.T) {
	for _, mode := range []Mode{ModeTest, ModeLive} {
		key, err := Generate(mode)
		if err != nil {
			t.Fatal(err)
		}
		if !regexp.MustCompile(`^sk_` + string(mode) + `_[0-9a-f]{48}$`).MatchString(key.Secret) {
			t.Fatalf("secret %q has the wrong format", key.Secret)
		}
		if key.Prefix != key.Secret[:12] {
			t.Fatalf("prefix = %q, want first 12 chars of secret", key.Prefix)
		}
		if !bytes.Equal(key.Hash, Hash(key.Secret)) {
			t.Fatal("hash does not match secret")
		}
	}

	a, _ := Generate(ModeTest)
	b, _ := Generate(ModeTest)
	if a.Secret == b.Secret {
		t.Fatal("two generated keys are identical")
	}
}

func TestParseMode(t *testing.T) {
	for _, s := range []string{"test", "live"} {
		if _, err := ParseMode(s); err != nil {
			t.Errorf("ParseMode(%q) = %v", s, err)
		}
	}
	if _, err := ParseMode("prod"); err == nil {
		t.Error("ParseMode(\"prod\") succeeded, want error")
	}
}

func TestLooksLikeKey(t *testing.T) {
	tests := map[string]bool{
		"sk_test_abc": true,
		"sk_live_abc": true,
		"pk_test_abc": false,
		"sk_prod_abc": false,
		"":            false,
	}
	for in, want := range tests {
		if got := LooksLikeKey(in); got != want {
			t.Errorf("LooksLikeKey(%q) = %v, want %v", in, got, want)
		}
	}
}
