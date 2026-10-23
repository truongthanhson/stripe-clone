package main

import (
	"bytes"
	"context"
	"encoding/json"
	"mime"
	"net/http"

	"github.com/truongthanhson/stripe-clone/internal/apiversion"
)

const versionHeader = "Stripe-Version"

type versionKey struct{}

// versionFrom returns the API version the current request runs under.
func versionFrom(ctx context.Context) string {
	if v, ok := ctx.Value(versionKey{}).(string); ok {
		return v
	}
	return apiversion.Latest()
}

// withAPIVersion picks the request's API version: the Stripe-Version header if sent, otherwise the
// version the merchant is pinned to. Handlers write the latest shape; for an older version the
// JSON response is rewritten by apiversion.Downgrade before it is sent.
//
// It wraps idempotency, so a stored response is the latest shape and a replay is downgraded for
// whichever version the retry asks for.
func withAPIVersion(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		version := principalFrom(r.Context()).APIVersion
		if h := r.Header.Get(versionHeader); h != "" {
			version = h
		}
		if version == "" {
			version = apiversion.Latest()
		}
		if !apiversion.Valid(version) {
			writeError(w, http.StatusBadRequest, apiError{Type: "invalid_request_error", Code: "api_version_invalid",
				Message: "Invalid API version: '" + version + "'. Released versions are " + joinVersions() + "."})
			return
		}
		w.Header().Set(versionHeader, version)
		r = r.WithContext(context.WithValue(r.Context(), versionKey{}, version))

		if version == apiversion.Latest() {
			next.ServeHTTP(w, r)
			return
		}

		rec := &bufferedResponse{header: w.Header(), status: http.StatusOK}
		next.ServeHTTP(rec, r)
		body := rec.body.Bytes()
		if mt, _, _ := mime.ParseMediaType(w.Header().Get("Content-Type")); mt == "application/json" {
			body = downgrade(body, version)
		}
		w.WriteHeader(rec.status)
		_, _ = w.Write(body)
	})
}

func downgrade(body []byte, version string) []byte {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber() // keep amounts exact; float64 would lose precision above 2^53
	var v any
	if err := dec.Decode(&v); err != nil {
		return body
	}
	apiversion.Downgrade(v, version)
	var out bytes.Buffer
	if err := json.NewEncoder(&out).Encode(v); err != nil {
		return body
	}
	return out.Bytes()
}

func joinVersions() string {
	var s string
	for i, v := range apiversion.All() {
		if i > 0 {
			s += ", "
		}
		s += v
	}
	return s
}
