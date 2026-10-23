// Package metadata implements the key-value metadata merchants attach to objects, with Stripe's limits.
package metadata

import (
	"fmt"
	"maps"
	"strings"
)

// Error names the offending parameter, e.g. metadata[order_id]. The API layer maps it like a
// ValidationError of any resource.
type Error struct {
	Param   string
	Message string
}

func (e *Error) Error() string { return e.Param + ": " + e.Message }

// Metadata limits, as Stripe's: up to 50 keys, keys up to 40 characters, values up to 500.
const (
	MaxKeys     = 50
	MaxKeyLen   = 40
	MaxValueLen = 500
)

// Update is a change to a metadata map: Clear empties it first, then Set adds or replaces
// keys and Unset removes them. Sending metadata[key]= (empty) unsets a key, metadata= clears all.
type Update struct {
	Clear bool
	Set   map[string]string
	Unset []string
}

// Apply returns the updated copy of current, or a ValidationError if the result breaks the limits.
func (u Update) Apply(current map[string]string) (map[string]string, error) {
	out := map[string]string{}
	if !u.Clear {
		maps.Copy(out, current)
	}
	for k, v := range u.Set {
		if err := validatePair(k, v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	for _, k := range u.Unset {
		delete(out, k)
	}
	if len(out) > MaxKeys {
		return nil, &Error{"metadata", fmt.Sprintf("Metadata can have at most %d keys.", MaxKeys)}
	}
	return out, nil
}

func validatePair(k, v string) error {
	param := "metadata[" + k + "]"
	switch {
	case k == "" || len(k) > MaxKeyLen || strings.ContainsAny(k, "[]"):
		return &Error{param, fmt.Sprintf("Metadata keys must be 1 to %d characters without square brackets.", MaxKeyLen)}
	case len(v) > MaxValueLen:
		return &Error{param, fmt.Sprintf("Metadata values can be at most %d characters.", MaxValueLen)}
	}
	return nil
}
