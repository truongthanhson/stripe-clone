package main

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/truongthanhson/stripe-clone/internal/checkout"
	"github.com/truongthanhson/stripe-clone/internal/events"
	"github.com/truongthanhson/stripe-clone/internal/listing"
	"github.com/truongthanhson/stripe-clone/internal/paymentintent"
	"github.com/truongthanhson/stripe-clone/internal/refund"
)

type eventStore interface {
	Get(ctx context.Context, merchantID int64, livemode bool, id string) (events.Event, error)
	List(ctx context.Context, merchantID int64, livemode bool, typ string, p listing.Params) ([]events.Event, bool, error)
}

// intentEventType names the event for a PaymentIntent change; "" means the change is not announced
// (such as recording an authorization while still processing).
func intentEventType(pi paymentintent.PaymentIntent, from paymentintent.Status) string {
	if from == "" {
		return "payment_intent.created"
	}
	if from == pi.Status {
		return ""
	}
	switch pi.Status {
	case paymentintent.StatusProcessing:
		return "payment_intent.processing"
	case paymentintent.StatusRequiresCapture:
		return "payment_intent.amount_capturable_updated"
	case paymentintent.StatusSucceeded:
		return "payment_intent.succeeded"
	case paymentintent.StatusCanceled:
		return "payment_intent.canceled"
	case paymentintent.StatusRequiresPaymentMethod:
		if from == paymentintent.StatusProcessing {
			return "payment_intent.payment_failed"
		}
	}
	return ""
}

func recordIntentEvent(ctx context.Context, tx pgx.Tx, pi paymentintent.PaymentIntent, from paymentintent.Status) error {
	typ := intentEventType(pi, from)
	if typ == "" {
		return nil
	}
	return events.Record(ctx, tx, pi.MerchantID, pi.Livemode, typ, renderIntent(pi))
}

func recordRefundEvent(ctx context.Context, tx pgx.Tx, re refund.Refund) error {
	typ := "refund.created"
	switch re.Status {
	case refund.StatusSucceeded:
		typ = "refund.updated"
	case refund.StatusFailed:
		typ = "refund.failed"
	}
	return events.Record(ctx, tx, re.MerchantID, re.Livemode, typ, renderRefund(re))
}

func recordCheckoutEvent(publicURL string) checkout.Hook {
	return func(ctx context.Context, tx pgx.Tx, s checkout.Session) error {
		typ := "checkout.session.expired"
		if s.Status == checkout.StatusComplete {
			typ = "checkout.session.completed"
		}
		return events.Record(ctx, tx, s.MerchantID, s.Livemode, typ, renderCheckoutSession(s, publicURL))
	}
}

func noSuchEvent(id string) apiError {
	return apiError{Type: "invalid_request_error", Code: "resource_missing", Param: "id", Message: "No such event: '" + id + "'"}
}

func handleGetEvent(store eventStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := principalFrom(r.Context())
		ev, err := store.Get(r.Context(), p.MerchantID, p.Livemode(), r.PathValue("id"))
		switch {
		case errors.Is(err, events.ErrNotFound):
			writeError(w, http.StatusNotFound, noSuchEvent(r.PathValue("id")))
		case err != nil:
			writeInternalError(w, r, "get event", err)
		default:
			writeJSON(w, http.StatusOK, ev.JSON())
		}
	}
}

func handleListEvents(store eventStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params, apiErr := parseListParams(r)
		if apiErr != nil {
			writeError(w, http.StatusBadRequest, *apiErr)
			return
		}
		p := principalFrom(r.Context())
		page, hasMore, err := store.List(r.Context(), p.MerchantID, p.Livemode(), r.URL.Query().Get("type"), params)
		if errors.Is(err, events.ErrNotFound) {
			id, _ := params.Cursor()
			e := noSuchEvent(id)
			e.Param = cursorParam(params)
			writeError(w, http.StatusNotFound, e)
			return
		}
		if err != nil {
			writeInternalError(w, r, "list events", err)
			return
		}
		out := listJSON[events.JSON]{Object: "list", URL: "/v1/events", HasMore: hasMore, Data: []events.JSON{}}
		for _, ev := range page {
			out.Data = append(out.Data, ev.JSON())
		}
		writeJSON(w, http.StatusOK, out)
	}
}
