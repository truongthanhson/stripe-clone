package paymentintent

import (
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"
)

func TestMetadataUpdateApply(t *testing.T) {
	current := map[string]string{"order_id": "6735", "channel": "web"}

	tests := []struct {
		name string
		u    MetadataUpdate
		want map[string]string
	}{
		{"no change", MetadataUpdate{}, current},
		{"set adds and replaces", MetadataUpdate{Set: map[string]string{"channel": "app", "sku": "A1"}},
			map[string]string{"order_id": "6735", "channel": "app", "sku": "A1"}},
		{"unset removes", MetadataUpdate{Unset: []string{"channel"}}, map[string]string{"order_id": "6735"}},
		{"clear then set", MetadataUpdate{Clear: true, Set: map[string]string{"x": "1"}}, map[string]string{"x": "1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.u.Apply(current)
			if err != nil {
				t.Fatal(err)
			}
			if !maps.Equal(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
	if current["channel"] != "web" || len(current) != 2 {
		t.Fatal("Apply modified its input")
	}
}

func TestMetadataLimits(t *testing.T) {
	tooMany := map[string]string{}
	for i := range MaxMetadataKeys + 1 {
		tooMany[fmt.Sprintf("k%d", i)] = "v"
	}
	cases := map[string]MetadataUpdate{
		"51 keys":     {Set: tooMany},
		"long key":    {Set: map[string]string{strings.Repeat("k", 41): "v"}},
		"long value":  {Set: map[string]string{"k": strings.Repeat("v", 501)}},
		"bracket key": {Set: map[string]string{"a[b": "v"}},
	}
	for name, u := range cases {
		var ve *ValidationError
		if _, err := u.Apply(nil); !errors.As(err, &ve) || ve.Code != "parameter_invalid_metadata" {
			t.Errorf("%s: err = %v, want parameter_invalid_metadata", name, err)
		}
	}

	exactly50 := map[string]string{}
	for i := range MaxMetadataKeys {
		exactly50[fmt.Sprintf("k%d", i)] = strings.Repeat("v", MaxMetadataValLen)
	}
	if _, err := (MetadataUpdate{Set: exactly50}).Apply(nil); err != nil {
		t.Errorf("50 keys at max length: %v", err)
	}
}
