// Package apiversion implements dated API versions, the way Stripe keeps old integrations working.
//
// Handlers always produce the latest shape. Each breaking change is registered here once, with a
// function that turns a response object back into the shape of the version before it. A request
// pinned to an older version gets the latest response with every newer change undone, newest first.
// Old integrations keep working, and handlers never branch on versions.
package apiversion

import (
	"slices"
)

// Versions, oldest first. A new version is a new constant, appended here, plus its changes below.
const (
	V2026_10_01 = "2026-10-01" // first public version
	V2026_10_23 = "2026-10-23"
)

var versions = []string{V2026_10_01, V2026_10_23}

// Latest is the version new merchants are pinned to.
func Latest() string { return versions[len(versions)-1] }

// Valid reports whether v is a released version.
func Valid(v string) bool { return slices.Contains(versions, v) }

// All lists released versions, oldest first.
func All() []string { return slices.Clone(versions) }

// change is one breaking change: from Version on, objects of type Object look different, and
// Downgrade rewrites one back to how earlier versions showed it.
type change struct {
	Version     string
	Object      string // the "object" field of the resources it touches, e.g. "payment_intent"
	Description string
	Downgrade   func(obj map[string]any)
}

var changes = []change{
	{
		Version: V2026_10_23,
		Object:  "payment_intent",
		Description: "PaymentIntents created without capture_method default to automatic_async instead of automatic. " +
			"Earlier versions see automatic_async reported as automatic (the two capture identically here).",
		Downgrade: func(pi map[string]any) {
			if pi["capture_method"] == "automatic_async" {
				pi["capture_method"] = "automatic"
			}
		},
	},
}

// DefaultCaptureMethod is the capture_method a PaymentIntent gets when the request omits it.
func DefaultCaptureMethod(version string) string {
	if before(version, V2026_10_23) {
		return "automatic"
	}
	return "automatic_async"
}

// Downgrade rewrites a decoded JSON response (maps, slices and scalars from encoding/json) in place
// so it matches version. Objects nested anywhere, such as list data or the payment_intent inside a
// card error, are rewritten too.
func Downgrade(body any, version string) {
	for i := len(changes) - 1; i >= 0; i-- {
		c := changes[i]
		if before(version, c.Version) {
			walk(body, c)
		}
	}
}

func walk(v any, c change) {
	switch v := v.(type) {
	case map[string]any:
		for _, child := range v {
			walk(child, c)
		}
		if v["object"] == c.Object {
			c.Downgrade(v)
		}
	case []any:
		for _, child := range v {
			walk(child, c)
		}
	}
}

// before reports whether version a was released before b. Dates in YYYY-MM-DD order as strings.
func before(a, b string) bool { return a < b }
