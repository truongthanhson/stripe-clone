package metadata

import (
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"
)

func TestApply(t *testing.T) {
	current := map[string]string{"order_id": "6735", "channel": "web"}

	tests := []struct {
		name string
		u    Update
		want map[string]string
	}{
		{"no change", Update{}, current},
		{"set adds and replaces", Update{Set: map[string]string{"channel": "app", "sku": "A1"}},
			map[string]string{"order_id": "6735", "channel": "app", "sku": "A1"}},
		{"unset removes", Update{Unset: []string{"channel"}}, map[string]string{"order_id": "6735"}},
		{"clear then set", Update{Clear: true, Set: map[string]string{"x": "1"}}, map[string]string{"x": "1"}},
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
	for i := range MaxKeys + 1 {
		tooMany[fmt.Sprintf("k%d", i)] = "v"
	}
	cases := map[string]Update{
		"51 keys":     {Set: tooMany},
		"long key":    {Set: map[string]string{strings.Repeat("k", 41): "v"}},
		"long value":  {Set: map[string]string{"k": strings.Repeat("v", 501)}},
		"bracket key": {Set: map[string]string{"a[b": "v"}},
	}
	for name, u := range cases {
		var me *Error
		if _, err := u.Apply(nil); !errors.As(err, &me) || !strings.HasPrefix(me.Param, "metadata") {
			t.Errorf("%s: err = %v, want a metadata Error", name, err)
		}
	}

	exactly50 := map[string]string{}
	for i := range MaxKeys {
		exactly50[fmt.Sprintf("k%d", i)] = strings.Repeat("v", MaxValueLen)
	}
	if _, err := (Update{Set: exactly50}).Apply(nil); err != nil {
		t.Errorf("50 keys at max length: %v", err)
	}
}
