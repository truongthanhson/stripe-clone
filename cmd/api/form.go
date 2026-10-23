package main

import (
	"errors"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/truongthanhson/stripe-clone/internal/paymentintent"
)

// maxBodyBytes bounds a request body; Stripe-style requests are small form posts.
const maxBodyBytes = 64 << 10

// parseForm reads an application/x-www-form-urlencoded body, as Stripe's API takes, and rejects
// parameters the endpoint does not know, so a typo fails loudly instead of being ignored.
func parseForm(w http.ResponseWriter, r *http.Request, allowed ...string) (url.Values, *apiError) {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mt, _, _ := mime.ParseMediaType(ct)
		if mt != "application/x-www-form-urlencoded" {
			return nil, &apiError{
				Type:    "invalid_request_error",
				Code:    "content_type_invalid",
				Message: "Send parameters as application/x-www-form-urlencoded, for example: curl -d amount=2000 -d currency=usd.",
			}
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := r.ParseForm(); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, &apiError{Type: "invalid_request_error", Code: "request_too_large", Message: "The request body is too large."}
		}
		return nil, &apiError{Type: "invalid_request_error", Code: "parameter_invalid", Message: "The request body could not be parsed."}
	}
	form := r.PostForm
	for name := range form {
		if !isAllowed(name, allowed) {
			return nil, &apiError{
				Type:    "invalid_request_error",
				Code:    "parameter_unknown",
				Param:   name,
				Message: "Received unknown parameter: " + name,
			}
		}
	}
	return form, nil
}

// isAllowed accepts an exact name, or name[key] for hash parameters such as metadata[order_id].
func isAllowed(name string, allowed []string) bool {
	for _, a := range allowed {
		if name == a || (strings.HasPrefix(name, a+"[") && strings.HasSuffix(name, "]")) {
			return true
		}
	}
	return false
}

// parseMetadata reads Stripe's hash syntax: metadata[key]=value sets a key, metadata[key]= unsets it,
// and metadata= (empty) clears every key.
func parseMetadata(form url.Values) paymentintent.MetadataUpdate {
	var u paymentintent.MetadataUpdate
	for name, values := range form {
		if name == "metadata" {
			u.Clear = values[0] == ""
			continue
		}
		key, ok := strings.CutPrefix(name, "metadata[")
		if !ok {
			continue
		}
		key = strings.TrimSuffix(key, "]")
		if values[0] == "" {
			u.Unset = append(u.Unset, key)
			continue
		}
		if u.Set == nil {
			u.Set = map[string]string{}
		}
		u.Set[key] = values[0]
	}
	return u
}

func hasMetadata(form url.Values) bool {
	for name := range form {
		if name == "metadata" || strings.HasPrefix(name, "metadata[") {
			return true
		}
	}
	return false
}

// optionalString returns nil when the parameter is absent, so "not sent" and "sent empty" stay distinct.
func optionalString(form url.Values, name string) *string {
	if !form.Has(name) {
		return nil
	}
	v := form.Get(name)
	return &v
}

func requiredInt(form url.Values, name string) (int64, *apiError) {
	if !form.Has(name) || form.Get(name) == "" {
		return 0, &apiError{Type: "invalid_request_error", Code: "parameter_missing", Param: name,
			Message: "Missing required param: " + name + "."}
	}
	n, err := strconv.ParseInt(form.Get(name), 10, 64)
	if err != nil {
		return 0, &apiError{Type: "invalid_request_error", Code: "parameter_invalid_integer", Param: name,
			Message: "Invalid integer: " + form.Get(name)}
	}
	return n, nil
}

func requiredString(form url.Values, name string) (string, *apiError) {
	if form.Get(name) == "" {
		return "", &apiError{Type: "invalid_request_error", Code: "parameter_missing", Param: name,
			Message: "Missing required param: " + name + "."}
	}
	return form.Get(name), nil
}

func optionalBool(form url.Values, name string) (bool, *apiError) {
	switch form.Get(name) {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	}
	return false, &apiError{Type: "invalid_request_error", Code: "parameter_invalid_boolean", Param: name,
		Message: "Invalid boolean: " + form.Get(name)}
}
