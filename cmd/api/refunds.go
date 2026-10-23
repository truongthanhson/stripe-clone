package main

import (
	"context"
	"errors"
	"net/http"

	"github.com/truongthanhson/stripe-clone/internal/listing"
	"github.com/truongthanhson/stripe-clone/internal/metadata"
	"github.com/truongthanhson/stripe-clone/internal/refund"
)

type refundStore interface {
	Create(ctx context.Context, merchantID int64, livemode bool, p refund.CreateParams) (refund.Refund, error)
	Get(ctx context.Context, merchantID int64, livemode bool, id string) (refund.Refund, error)
	List(ctx context.Context, merchantID int64, livemode bool, paymentIntentID string, p listing.Params) ([]refund.Refund, bool, error)
}

type refundJSON struct {
	ID            string            `json:"id"`
	Object        string            `json:"object"`
	Amount        int64             `json:"amount"`
	Created       int64             `json:"created"`
	Currency      string            `json:"currency"`
	FailureReason *string           `json:"failure_reason"`
	Metadata      map[string]string `json:"metadata"`
	PaymentIntent string            `json:"payment_intent"`
	Reason        *string           `json:"reason"`
	Status        string            `json:"status"`
}

func renderRefund(re refund.Refund) refundJSON {
	md := re.Metadata
	if md == nil {
		md = map[string]string{}
	}
	return refundJSON{
		ID: re.ID, Object: "refund", Amount: re.Amount, Created: re.Created.Unix(), Currency: re.Currency,
		FailureReason: re.FailureReason, Metadata: md, PaymentIntent: re.PaymentIntentID, Reason: re.Reason,
		Status: string(re.Status),
	}
}

func handleCreateRefund(store refundStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		form, apiErr := parseForm(w, r)
		if apiErr != nil {
			writeError(w, http.StatusBadRequest, *apiErr)
			return
		}
		params := refund.CreateParams{
			PaymentIntentID: form.Get("payment_intent"),
			Reason:          optionalString(form, "reason"),
			Metadata:        parseMetadata(form),
		}
		if form.Has("amount") {
			n, apiErr := requiredInt(form, "amount")
			if apiErr != nil {
				writeError(w, http.StatusBadRequest, *apiErr)
				return
			}
			params.Amount = &n
		}
		p := principalFrom(r.Context())
		re, err := store.Create(r.Context(), p.MerchantID, p.Livemode(), params)
		if err != nil {
			writeRefundError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, renderRefund(re))
	}
}

func handleGetRefund(store refundStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := principalFrom(r.Context())
		re, err := store.Get(r.Context(), p.MerchantID, p.Livemode(), r.PathValue("id"))
		if err != nil {
			writeRefundError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, renderRefund(re))
	}
}

func handleListRefunds(store refundStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params, apiErr := parseListParams(r)
		if apiErr != nil {
			writeError(w, http.StatusBadRequest, *apiErr)
			return
		}
		p := principalFrom(r.Context())
		page, hasMore, err := store.List(r.Context(), p.MerchantID, p.Livemode(), r.URL.Query().Get("payment_intent"), params)
		if errors.Is(err, refund.ErrNotFound) {
			id, _ := params.Cursor()
			e := noSuchRefund(id)
			e.Param = cursorParam(params)
			writeError(w, http.StatusNotFound, e)
			return
		}
		if err != nil {
			writeRefundError(w, r, err)
			return
		}
		out := listJSON[refundJSON]{Object: "list", URL: "/v1/refunds", HasMore: hasMore, Data: []refundJSON{}}
		for _, re := range page {
			out.Data = append(out.Data, renderRefund(re))
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func noSuchRefund(id string) apiError {
	return apiError{Type: "invalid_request_error", Code: "resource_missing", Param: "id", Message: "No such refund: '" + id + "'"}
}

func writeRefundError(w http.ResponseWriter, r *http.Request, err error) {
	var (
		ve *refund.ValidationError
		me *metadata.Error
	)
	switch {
	case errors.As(err, &ve) && ve.Code == "resource_missing":
		writeError(w, http.StatusNotFound, apiError{Type: "invalid_request_error", Code: ve.Code, Param: ve.Param, Message: ve.Message})
	case errors.As(err, &ve):
		writeError(w, http.StatusBadRequest, apiError{Type: "invalid_request_error", Code: ve.Code, Param: ve.Param, Message: ve.Message})
	case errors.As(err, &me):
		writeError(w, http.StatusBadRequest, apiError{Type: "invalid_request_error", Code: "parameter_invalid_metadata", Param: me.Param, Message: me.Message})
	case errors.Is(err, refund.ErrNotFound):
		writeError(w, http.StatusNotFound, noSuchRefund(r.PathValue("id")))
	default:
		writeInternalError(w, r, "refund", err)
	}
}
