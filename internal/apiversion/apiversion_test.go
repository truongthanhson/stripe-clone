package apiversion

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestVersionsAreOrderedDates(t *testing.T) {
	if !slices.IsSorted(versions) {
		t.Fatalf("versions must be listed oldest first: %v", versions)
	}
	for _, v := range versions {
		if len(v) != len("2006-01-02") {
			t.Errorf("version %q is not YYYY-MM-DD", v)
		}
	}
	for _, c := range changes {
		if !Valid(c.Version) || c.Version == versions[0] {
			t.Errorf("change %q belongs to %s, which is not a released version after the first", c.Description, c.Version)
		}
	}
}

func TestDowngrade(t *testing.T) {
	body := func() any {
		var v any
		_ = json.Unmarshal([]byte(`{
			"object": "list",
			"data": [
				{"object": "payment_intent", "id": "pi_1", "capture_method": "automatic_async"},
				{"object": "payment_intent", "id": "pi_2", "capture_method": "manual"}
			],
			"error": {"payment_intent": {"object": "payment_intent", "capture_method": "automatic_async"}},
			"other": {"object": "refund", "capture_method": "automatic_async"}
		}`), &v)
		return v
	}
	captureMethods := func(v any) []any {
		m := v.(map[string]any)
		data := m["data"].([]any)
		return []any{
			data[0].(map[string]any)["capture_method"],
			data[1].(map[string]any)["capture_method"],
			m["error"].(map[string]any)["payment_intent"].(map[string]any)["capture_method"],
			m["other"].(map[string]any)["capture_method"],
		}
	}

	latest := body()
	Downgrade(latest, Latest())
	if got := captureMethods(latest); !slices.Equal(got, []any{"automatic_async", "manual", "automatic_async", "automatic_async"}) {
		t.Fatalf("latest changed: %v", got)
	}

	old := body()
	Downgrade(old, V2026_10_01)
	if got := captureMethods(old); !slices.Equal(got, []any{"automatic", "manual", "automatic", "automatic_async"}) {
		t.Fatalf("2026-10-01: %v (only payment_intent objects change, nested ones included)", got)
	}
}

func TestDefaultCaptureMethod(t *testing.T) {
	if got := DefaultCaptureMethod(V2026_10_01); got != "automatic" {
		t.Errorf("2026-10-01: %s", got)
	}
	if got := DefaultCaptureMethod(V2026_10_23); got != "automatic_async" {
		t.Errorf("2026-10-23: %s", got)
	}
}
