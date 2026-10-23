package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/truongthanhson/stripe-clone/internal/idempotency"
)

type idempotencyStore interface {
	Begin(ctx context.Context, sc idempotency.Scope, fingerprint []byte) (*idempotency.Response, error)
	Complete(ctx context.Context, sc idempotency.Scope, resp idempotency.Response) error
	Release(ctx context.Context, sc idempotency.Scope) error
}

// withIdempotency runs a POST at most once per Idempotency-Key. It sits after requireAPIKey,
// because keys belong to a merchant and mode.
//
// Which outcomes are stored: anything the request decided (2xx, 402 card declined, 404, ...) is
// replayed on retry. A 400 (bad parameters or wrong state), a 409, a 429 or a 5xx releases the
// key so the client can fix the problem or try again with the same key; every handler is safe to
// run again because the state machine refuses a second confirm or capture.
func withIdempotency(store idempotencyStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if store == nil || key == "" || r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		if len(key) > idempotency.MaxKeyLength {
			writeError(w, http.StatusBadRequest, apiError{Type: "idempotency_error", Code: "idempotency_key_too_long",
				Message: "Idempotency-Key must be at most 255 characters."})
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			writeError(w, http.StatusBadRequest, apiError{Type: "invalid_request_error", Code: "request_too_large",
				Message: "The request body is too large."})
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		p := principalFrom(r.Context())
		sc := idempotency.Scope{MerchantID: p.MerchantID, Livemode: p.Livemode(), Key: key}
		w.Header().Set("Idempotency-Key", key)

		stored, err := store.Begin(r.Context(), sc, idempotency.Fingerprint(r.Method, r.URL.Path, body))
		switch {
		case errors.Is(err, idempotency.ErrMismatch):
			writeError(w, http.StatusBadRequest, apiError{Type: "idempotency_error", Code: "idempotency_key_in_use",
				Message: "Keys for idempotent requests can only be used with the same parameters they were first used with. " +
					"Try using a key other than '" + key + "' if you meant to execute a different request."})
			return
		case errors.Is(err, idempotency.ErrInProgress):
			writeError(w, http.StatusConflict, apiError{Type: "idempotency_error", Code: "idempotency_key_in_use",
				Message: "There is currently another in-progress request using this Idempotency-Key. Retry after it completes."})
			return
		case err != nil:
			writeInternalError(w, r, "idempotency begin", err)
			return
		case stored != nil:
			w.Header().Set("Idempotent-Replayed", "true")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(stored.Status)
			_, _ = w.Write(stored.Body)
			return
		}

		rec := &bufferedResponse{header: w.Header(), status: http.StatusOK}
		next.ServeHTTP(rec, r)

		// The request's own context may be canceled once the client goes away; the key must still be settled.
		ctx := context.WithoutCancel(r.Context())
		if releasable(rec.status) {
			err = store.Release(ctx, sc)
		} else {
			err = store.Complete(ctx, sc, idempotency.Response{Status: rec.status, Body: rec.body.Bytes()})
		}
		if err != nil {
			loggerFrom(r.Context()).Error("idempotency settle", "error", err)
		}

		w.WriteHeader(rec.status)
		_, _ = w.Write(rec.body.Bytes())
	})
}

func releasable(status int) bool {
	return status == http.StatusBadRequest || status == http.StatusConflict ||
		status == http.StatusTooManyRequests || status >= 500
}

// bufferedResponse holds the handler's response so it can be stored before it is sent.
type bufferedResponse struct {
	header      http.Header
	status      int
	wroteHeader bool
	body        bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header { return b.header }

func (b *bufferedResponse) WriteHeader(code int) {
	if !b.wroteHeader {
		b.status = code
		b.wroteHeader = true
	}
}

func (b *bufferedResponse) Write(p []byte) (int, error) {
	b.wroteHeader = true
	return b.body.Write(p)
}
