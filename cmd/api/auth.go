package main

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/truongthanhson/stripe-clone/internal/apikey"
)

type keyLookup interface {
	Lookup(ctx context.Context, secret string) (apikey.Principal, error)
}

type principalKey struct{}

func principalFrom(ctx context.Context) apikey.Principal {
	p, _ := ctx.Value(principalKey{}).(apikey.Principal)
	return p
}

var errInvalidKey = apiError{
	Type:    "invalid_request_error",
	Code:    "api_key_invalid",
	Message: "Invalid API key provided.",
}

// requireAPIKey authenticates "Authorization: Bearer sk_..." and puts the merchant and mode on the context.
func requireAPIKey(keys keyLookup, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secret, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || secret == "" {
			writeError(w, http.StatusUnauthorized, apiError{
				Type:    "invalid_request_error",
				Code:    "api_key_missing",
				Message: "You did not provide an API key. Send it as 'Authorization: Bearer sk_...'.",
			})
			return
		}
		if !apikey.LooksLikeKey(secret) {
			writeError(w, http.StatusUnauthorized, errInvalidKey)
			return
		}

		p, err := keys.Lookup(r.Context(), secret)
		switch {
		case errors.Is(err, apikey.ErrNotFound):
			writeError(w, http.StatusUnauthorized, errInvalidKey)
			return
		case err != nil:
			loggerFrom(r.Context()).Error("api key lookup failed", "error", err)
			writeError(w, http.StatusInternalServerError, apiError{
				Type:    "api_error",
				Message: "Something went wrong on our end.",
			})
			return
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}
