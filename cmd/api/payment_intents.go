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
	Confirm(ctx context.Context, merchantID int64, livemode bool, id string, paymentMethod *string) (paymentintent.PaymentIntent, error)
	Capture(ctx context.Context, merchantID int64, livemode bool, id string, amount *int64) (paymentintent.PaymentIntent, error)
	Cancel(ctx context.Context, merchantID int64, livemode bool, id string, reason *string) (paymentintent.PaymentIntent, error)
	Update(ctx context.Context, merchantID int64, livemode bool, id string, p paymentintent.UpdateParams) (paymentintent.PaymentIntent, error)
	List(ctx context.Context, merchantID int64, livemode bool, p paymentintent.ListParams) ([]paymentintent.PaymentIntent, bool, error)
}

// intentJSON is the public representation of a PaymentIntent; field names and order follow Stripe's.
type intentJSON struct {
	ID                 string                      `json:"id"`
	Object             string                      `json:"object"`
	Amount             int64                       `json:"amount"`
	AmountCapturable   int64                       `json:"amount_capturable"`
	AmountReceived     int64                       `json:"amount_received"`
	CanceledAt         *int64                      `json:"canceled_at"`
	CancellationReason *string                     `json:"cancellation_reason"`
	CaptureMethod      string                      `json:"capture_method"`
	Created            int64                       `json:"created"`
	Currency           string                      `json:"currency"`
	Description        *string                     `json:"description"`
	LastPaymentError   *paymentintent.PaymentError `json:"last_payment_error"`
	Livemode           bool                        `json:"livemode"`
	Metadata           map[string]string           `json:"metadata"`
	PaymentMethod      *string                     `json:"payment_method"`
	Status             string                      `json:"status"`
}

func renderIntent(pi paymentintent.PaymentIntent) intentJSON {
	var canceledAt *int64
	if pi.CanceledAt != nil {
		t := pi.CanceledAt.Unix()
		canceledAt = &t
	}
	metadata := pi.Metadata
	if metadata == nil {
		metadata = map[string]string{} // always an object, never null
	}
	return intentJSON{
		ID:                 pi.ID,
		Object:             "payment_intent",
		Amount:             pi.Amount,
		AmountCapturable:   pi.AmountCapturable,
		AmountReceived:     pi.AmountReceived,
		CanceledAt:         canceledAt,
		CancellationReason: pi.CancellationReason,
		CaptureMethod:      string(pi.CaptureMethod),
		Created:            pi.Created.Unix(),
		Currency:           pi.Currency,
		Description:        pi.Description,
		LastPaymentError:   pi.LastPaymentError,
		Livemode:           pi.Livemode,
		Metadata:           metadata,
		PaymentMethod:      pi.PaymentMethod,
		Status:             string(pi.Status),
	}
}

func handleCreateIntent(store intentStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		form, apiErr := parseForm(w, r, "amount", "currency", "capture_method", "confirm", "description", "metadata", "payment_method")
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
			Metadata:      parseMetadata(form),
		}
		if pm := params.PaymentMethod; pm != nil && !strings.HasPrefix(*pm, "pm_") {
			writeError(w, http.StatusBadRequest, apiError{Type: "invalid_request_error", Code: "resource_missing",
				Param: "payment_method", Message: "No such PaymentMethod: '" + *pm + "'"})
			return
		}

		confirm, apiErr := optionalBool(form, "confirm")
		if apiErr != nil {
			writeError(w, http.StatusBadRequest, *apiErr)
			return
		}
		if confirm && params.PaymentMethod == nil {
			writeError(w, http.StatusBadRequest, apiError{Type: "invalid_request_error", Code: "parameter_missing",
				Param: "payment_method", Message: "confirm=true requires a payment_method."})
			return
		}

		p := principalFrom(r.Context())
		pi, err := store.Create(r.Context(), p.MerchantID, p.Livemode(), params)
		if err != nil {
			writeIntentError(w, r, err)
			return
		}
		if confirm {
			pi, err = store.Confirm(r.Context(), p.MerchantID, p.Livemode(), pi.ID, nil)
			if err != nil {
				writeIntentError(w, r, err)
				return
			}
		}
		writeJSON(w, http.StatusOK, renderIntent(pi))
	}
}

func handleUpdateIntent(store intentStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		form, apiErr := parseForm(w, r, "description", "metadata")
		if apiErr != nil {
			writeError(w, http.StatusBadRequest, *apiErr)
			return
		}
		params := paymentintent.UpdateParams{Description: optionalString(form, "description")}
		if hasMetadata(form) {
			u := parseMetadata(form)
			params.Metadata = &u
		}
		p := principalFrom(r.Context())
		pi, err := store.Update(r.Context(), p.MerchantID, p.Livemode(), r.PathValue("id"), params)
		if err != nil {
			writeIntentError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, renderIntent(pi))
	}
}

// listJSON is Stripe's list envelope. Clients page by passing the last data[].id as starting_after
// while has_more is true.
type listJSON[T any] struct {
	Object  string `json:"object"`
	URL     string `json:"url"`
	HasMore bool   `json:"has_more"`
	Data    []T    `json:"data"`
}

func handleListIntents(store intentStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params, apiErr := parseListParams(r)
		if apiErr != nil {
			writeError(w, http.StatusBadRequest, *apiErr)
			return
		}
		p := principalFrom(r.Context())
		page, hasMore, err := store.List(r.Context(), p.MerchantID, p.Livemode(), params)
		if errors.Is(err, paymentintent.ErrNotFound) {
			param, id := "starting_after", params.StartingAfter
			if params.EndingBefore != "" {
				param, id = "ending_before", params.EndingBefore
			}
			e := noSuchIntent(id)
			e.Param = param
			writeError(w, http.StatusNotFound, e)
			return
		}
		if err != nil {
			writeIntentError(w, r, err)
			return
		}
		out := listJSON[intentJSON]{Object: "list", URL: "/v1/payment_intents", HasMore: hasMore, Data: []intentJSON{}}
		for _, pi := range page {
			out.Data = append(out.Data, renderIntent(pi))
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func handleConfirmIntent(store intentStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		form, apiErr := parseForm(w, r, "payment_method")
		if apiErr != nil {
			writeError(w, http.StatusBadRequest, *apiErr)
			return
		}
		p := principalFrom(r.Context())
		pi, err := store.Confirm(r.Context(), p.MerchantID, p.Livemode(), r.PathValue("id"), optionalString(form, "payment_method"))
		if err != nil {
			writeIntentError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, renderIntent(pi))
	}
}

func handleCaptureIntent(store intentStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		form, apiErr := parseForm(w, r, "amount_to_capture")
		if apiErr != nil {
			writeError(w, http.StatusBadRequest, *apiErr)
			return
		}
		var amount *int64
		if form.Has("amount_to_capture") {
			n, apiErr := requiredInt(form, "amount_to_capture")
			if apiErr != nil {
				writeError(w, http.StatusBadRequest, *apiErr)
				return
			}
			amount = &n
		}
		p := principalFrom(r.Context())
		pi, err := store.Capture(r.Context(), p.MerchantID, p.Livemode(), r.PathValue("id"), amount)
		if err != nil {
			writeIntentError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, renderIntent(pi))
	}
}

func handleCancelIntent(store intentStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		form, apiErr := parseForm(w, r, "cancellation_reason")
		if apiErr != nil {
			writeError(w, http.StatusBadRequest, *apiErr)
			return
		}
		p := principalFrom(r.Context())
		pi, err := store.Cancel(r.Context(), p.MerchantID, p.Livemode(), r.PathValue("id"), optionalString(form, "cancellation_reason"))
		if err != nil {
			writeIntentError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, renderIntent(pi))
	}
}

// writeIntentError maps the paymentintent package's errors to HTTP statuses and the API error shape.
func writeIntentError(w http.ResponseWriter, r *http.Request, err error) {
	var (
		ve      *paymentintent.ValidationError
		se      *paymentintent.StateError
		decline *paymentintent.DeclineError
	)
	switch {
	case errors.As(err, &ve):
		writeError(w, http.StatusBadRequest, apiError{Type: "invalid_request_error", Code: ve.Code, Param: ve.Param, Message: ve.Message})
	case errors.As(err, &se):
		writeError(w, http.StatusBadRequest, apiError{Type: "invalid_request_error", Code: "payment_intent_unexpected_state", Message: se.Error()})
	case errors.As(err, &decline):
		pe := decline.Intent.LastPaymentError
		intent := renderIntent(decline.Intent)
		writeError(w, http.StatusPaymentRequired, apiError{Type: pe.Type, Code: pe.Code, DeclineCode: pe.DeclineCode,
			Message: pe.Message, PaymentIntent: &intent})
	case errors.Is(err, paymentintent.ErrNotFound):
		writeError(w, http.StatusNotFound, noSuchIntent(r.PathValue("id")))
	case errors.Is(err, paymentintent.ErrNetworkUnavailable):
		writeError(w, http.StatusBadGateway, apiError{Type: "api_error", Code: "card_network_unavailable", Message: err.Error()})
	default:
		writeInternalError(w, r, "payment intent", err)
	}
}

func handleGetIntent(store intentStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := principalFrom(r.Context())
		pi, err := store.Get(r.Context(), p.MerchantID, p.Livemode(), r.PathValue("id"))
		if err != nil {
			writeIntentError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, renderIntent(pi))
	}
}

func noSuchIntent(id string) apiError {
	return apiError{Type: "invalid_request_error", Code: "resource_missing", Param: "intent",
		Message: "No such payment_intent: '" + id + "'"}
}
