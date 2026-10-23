package main

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/truongthanhson/stripe-clone/internal/paymentintent"
)

type intentStore interface {
	Create(ctx context.Context, merchantID int64, livemode bool, p paymentintent.CreateParams) (paymentintent.PaymentIntent, error)
	Get(ctx context.Context, merchantID int64, livemode bool, id string) (paymentintent.PaymentIntent, error)
}

// intentJSON is the public representation of a PaymentIntent; field names and order follow Stripe's.
type intentJSON struct {
	ID            string  `json:"id"`
	Object        string  `json:"object"`
	Amount        int64   `json:"amount"`
	CaptureMethod string  `json:"capture_method"`
	Created       int64   `json:"created"`
	Currency      string  `json:"currency"`
	Description   *string `json:"description"`
	Livemode      bool    `json:"livemode"`
	PaymentMethod *string `json:"payment_method"`
	Status        string  `json:"status"`
}

func renderIntent(pi paymentintent.PaymentIntent) intentJSON {
	return intentJSON{
		ID:            pi.ID,
		Object:        "payment_intent",
		Amount:        pi.Amount,
		CaptureMethod: string(pi.CaptureMethod),
		Created:       pi.Created.Unix(),
		Currency:      pi.Currency,
		Description:   pi.Description,
		Livemode:      pi.Livemode,
		PaymentMethod: pi.PaymentMethod,
		Status:        string(pi.Status),
	}
}

func handleCreateIntent(store intentStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		form, apiErr := parseForm(w, r, "amount", "currency", "capture_method", "description", "payment_method")
		if apiErr != nil {
			writeError(w, http.StatusBadRequest, *apiErr)
			return
		}
		amount, apiErr := requiredInt(form, "amount")
		if apiErr != nil {
			writeError(w, http.StatusBadRequest, *apiErr)
			return
		}
		currency, apiErr := requiredString(form, "currency")
		if apiErr != nil {
			writeError(w, http.StatusBadRequest, *apiErr)
			return
		}
		params := paymentintent.CreateParams{
			Amount:        amount,
			Currency:      strings.ToLower(currency),
			CaptureMethod: paymentintent.CaptureMethod(form.Get("capture_method")),
			Description:   optionalString(form, "description"),
			PaymentMethod: optionalString(form, "payment_method"),
		}
		if pm := params.PaymentMethod; pm != nil && !strings.HasPrefix(*pm, "pm_") {
			writeError(w, http.StatusBadRequest, apiError{Type: "invalid_request_error", Code: "resource_missing",
				Param: "payment_method", Message: "No such PaymentMethod: '" + *pm + "'"})
			return
		}

		p := principalFrom(r.Context())
		pi, err := store.Create(r.Context(), p.MerchantID, p.Livemode(), params)
		var ve *paymentintent.ValidationError
		switch {
		case errors.As(err, &ve):
			writeError(w, http.StatusBadRequest, apiError{Type: "invalid_request_error", Code: ve.Code, Param: ve.Param, Message: ve.Message})
			return
		case err != nil:
			writeInternalError(w, r, "create payment intent", err)
			return
		}
		writeJSON(w, http.StatusOK, renderIntent(pi))
	}
}

func handleGetIntent(store intentStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		p := principalFrom(r.Context())
		pi, err := store.Get(r.Context(), p.MerchantID, p.Livemode(), id)
		switch {
		case errors.Is(err, paymentintent.ErrNotFound):
			writeError(w, http.StatusNotFound, noSuchIntent(id))
			return
		case err != nil:
			writeInternalError(w, r, "get payment intent", err)
			return
		}
		writeJSON(w, http.StatusOK, renderIntent(pi))
	}
}

func noSuchIntent(id string) apiError {
	return apiError{Type: "invalid_request_error", Code: "resource_missing", Param: "intent",
		Message: "No such payment_intent: '" + id + "'"}
}
