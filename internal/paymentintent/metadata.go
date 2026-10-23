package paymentintent

import (
	"fmt"
	"maps"
	"strings"
)

// Metadata limits, as Stripe's: up to 50 keys, keys up to 40 characters, values up to 500.
const (
	MaxMetadataKeys   = 50
	MaxMetadataKeyLen = 40
	MaxMetadataValLen = 500
)

// MetadataUpdate is a change to a metadata map: Clear empties it first, then Set adds or replaces
// keys and Unset removes them. Sending metadata[key]= (empty) unsets a key, metadata= clears all.
type MetadataUpdate struct {
	Clear bool
	Set   map[string]string
	Unset []string
}

// Apply returns the updated copy of current, or a ValidationError if the result breaks the limits.
func (u MetadataUpdate) Apply(current map[string]string) (map[string]string, error) {
	out := map[string]string{}
	if !u.Clear {
		maps.Copy(out, current)
	}
	for k, v := range u.Set {
		if err := validateMetadataPair(k, v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	for _, k := range u.Unset {
		delete(out, k)
	}
	if len(out) > MaxMetadataKeys {
		return nil, &ValidationError{"metadata", "parameter_invalid_metadata",
			fmt.Sprintf("Metadata can have at most %d keys.", MaxMetadataKeys)}
	}
	return out, nil
}

func validateMetadataPair(k, v string) error {
	param := "metadata[" + k + "]"
	switch {
	case k == "" || len(k) > MaxMetadataKeyLen || strings.ContainsAny(k, "[]"):
		return &ValidationError{param, "parameter_invalid_metadata",
			fmt.Sprintf("Metadata keys must be 1 to %d characters without square brackets.", MaxMetadataKeyLen)}
	case len(v) > MaxMetadataValLen:
		return &ValidationError{param, "parameter_invalid_metadata",
			fmt.Sprintf("Metadata values can be at most %d characters.", MaxMetadataValLen)}
	}
	return nil
}
