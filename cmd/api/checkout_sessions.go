package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/truongthanhson/stripe-clone/internal/checkout"
	"github.com/truongthanhson/stripe-clone/internal/listing"
	"github.com/truongthanhson/stripe-clone/internal/metadata"
)

type checkoutStore interface {
	Create(ctx context.Context, merchantID int64, livemode bool, p checkout.CreateParams) (checkout.Session, error)
	Get(ctx context.Context, merchantID int64, livemode bool, id string) (checkout.Session, error)
	List(ctx context.Context, merchantID int64, livemode bool, p listing.Params) ([]checkout.Session, bool, error)
	Expire(ctx context.Context, merchantID int64, livemode bool, id string) (checkout.Session, error)
	Open(ctx context.Context, id string) (checkout.Session, error)
	Pay(ctx context.Context, id, email, paymentMethod string) (checkout.Session, error)
}

type customerDetailsJSON struct {
	Email *string `json:"email"`
}

// checkoutSessionJSON follows Stripe's Checkout Session; url is set only while the session is open.
type checkoutSessionJSON struct {
	ID                string               `json:"id"`
	Object            string               `json:"object"`
	AmountSubtotal    int64                `json:"amount_subtotal"`
	AmountTotal       int64                `json:"amount_total"`
	CancelURL         *string              `json:"cancel_url"`
	ClientReferenceID *string              `json:"client_reference_id"`
	Created           int64                `json:"created"`
	Currency          string               `json:"currency"`
	Customer          *string              `json:"customer"`
	CustomerDetails   *customerDetailsJSON `json:"customer_details"`
	CustomerEmail     *string              `json:"customer_email"`
	ExpiresAt         int64                `json:"expires_at"`
	Livemode          bool                 `json:"livemode"`
	Metadata          map[string]string    `json:"metadata"`
	Mode              string               `json:"mode"`
	PaymentIntent     string               `json:"payment_intent"`
	PaymentStatus     string               `json:"payment_status"`
	Status            string               `json:"status"`
	SuccessURL        string               `json:"success_url"`
	URL               *string              `json:"url"`
}

type lineItemJSON struct {
	ID          string `json:"id"`
	Object      string `json:"object"`
	AmountTotal int64  `json:"amount_total"`
	Currency    string `json:"currency"`
	Description string `json:"description"`
	Quantity    int64  `json:"quantity"`
	UnitAmount  int64  `json:"unit_amount"`
}

func renderCheckoutSession(s checkout.Session, publicURL string) checkoutSessionJSON {
	md := s.Metadata
	if md == nil {
		md = map[string]string{}
	}
	var pageURL *string
	if s.Status == checkout.StatusOpen {
		u := checkoutPageURL(publicURL, s.ID)
		pageURL = &u
	}
	var details *customerDetailsJSON
	if s.CustomerDetailsEmail != nil {
		details = &customerDetailsJSON{Email: s.CustomerDetailsEmail}
	}
	return checkoutSessionJSON{
		ID: s.ID, Object: "checkout.session", AmountSubtotal: s.AmountTotal, AmountTotal: s.AmountTotal,
		CancelURL: s.CancelURL, ClientReferenceID: s.ClientReferenceID, Created: s.Created.Unix(), Currency: s.Currency,
		Customer: s.Customer, CustomerDetails: details, CustomerEmail: s.CustomerEmail, ExpiresAt: s.ExpiresAt.Unix(),
		Livemode: s.Livemode, Metadata: md, Mode: "payment", PaymentIntent: s.PaymentIntentID,
		PaymentStatus: s.PaymentStatus(), Status: string(s.Status), SuccessURL: s.SuccessURL, URL: pageURL,
	}
}

func checkoutPageURL(publicURL, id string) string {
	return strings.TrimSuffix(publicURL, "/") + "/c/pay/" + id
}

var lineItemParam = regexp.MustCompile(`^line_items\[(\d+)\](\[price_data\]\[currency\]|\[price_data\]\[unit_amount\]|\[price_data\]\[product_data\]\[name\]|\[quantity\])$`)

// parseLineItems reads Stripe's array syntax, line_items[0][price_data][unit_amount]=2000. Indexes
// must run 0, 1, 2... without gaps, so a typo cannot silently drop an item.
func parseLineItems(form url.Values) ([]checkout.LineItem, *apiError) {
	byIndex := map[int]*checkout.LineItem{}
	for name, values := range form {
		if !strings.HasPrefix(name, "line_items[") {
			continue
		}
		m := lineItemParam.FindStringSubmatch(name)
		if m == nil {
			return nil, &apiError{Type: "invalid_request_error", Code: "parameter_unknown", Param: name, Message: "Received unknown parameter: " + name}
		}
		i, err := strconv.Atoi(m[1])
		if err != nil || i >= checkout.MaxLineItems {
			return nil, &apiError{Type: "invalid_request_error", Code: "parameter_invalid", Param: name,
				Message: fmt.Sprintf("You may pass at most %d line_items.", checkout.MaxLineItems)}
		}
		li := byIndex[i]
		if li == nil {
			li = &checkout.LineItem{}
			byIndex[i] = li
		}
		v := values[0]
		switch m[2] {
		case "[price_data][currency]":
			li.Currency = strings.ToLower(v)
		case "[price_data][product_data][name]":
			li.Name = v
		case "[price_data][unit_amount]", "[quantity]":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return nil, &apiError{Type: "invalid_request_error", Code: "parameter_invalid_integer", Param: name, Message: "Invalid integer: " + v}
			}
			if m[2] == "[quantity]" {
				li.Quantity = n
			} else {
				li.UnitAmount = n
			}
		}
	}
	items := make([]checkout.LineItem, len(byIndex))
	for i := range items {
		li, ok := byIndex[i]
		if !ok {
			return nil, &apiError{Type: "invalid_request_error", Code: "parameter_missing", Param: fmt.Sprintf("line_items[%d]", i),
				Message: fmt.Sprintf("Missing line_items[%d]: indexes must start at 0 and have no gaps.", i)}
		}
		if !form.Has(fmt.Sprintf("line_items[%d][price_data][unit_amount]", i)) {
			p := fmt.Sprintf("line_items[%d][price_data][unit_amount]", i)
			return nil, &apiError{Type: "invalid_request_error", Code: "parameter_missing", Param: p, Message: "Missing required param: " + p + "."}
		}
		if !form.Has(fmt.Sprintf("line_items[%d][quantity]", i)) {
			p := fmt.Sprintf("line_items[%d][quantity]", i)
			return nil, &apiError{Type: "invalid_request_error", Code: "parameter_missing", Param: p, Message: "Missing required param: " + p + "."}
		}
		items[i] = *li
	}
	return items, nil
}

func handleCreateCheckoutSession(store checkoutStore, publicURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		form, apiErr := parseForm(w, r)
		if apiErr != nil {
			writeError(w, http.StatusBadRequest, *apiErr)
			return
		}
		items, apiErr := parseLineItems(form)
		if apiErr != nil {
			writeError(w, http.StatusBadRequest, *apiErr)
			return
		}
		params := checkout.CreateParams{
			LineItems:         items,
			SuccessURL:        form.Get("success_url"),
			CancelURL:         optionalString(form, "cancel_url"),
			Customer:          optionalString(form, "customer"),
			CustomerEmail:     optionalString(form, "customer_email"),
			ClientReferenceID: optionalString(form, "client_reference_id"),
			Metadata:          parseMetadata(form),
		}
		if form.Has("expires_at") {
			n, apiErr := requiredInt(form, "expires_at")
			if apiErr != nil {
				writeError(w, http.StatusBadRequest, *apiErr)
				return
			}
			t := time.Unix(n, 0)
			params.ExpiresAt = &t
		}
		p := principalFrom(r.Context())
		s, err := store.Create(r.Context(), p.MerchantID, p.Livemode(), params)
		if err != nil {
			writeCheckoutError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, renderCheckoutSession(s, publicURL))
	}
}

func handleGetCheckoutSession(store checkoutStore, publicURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := principalFrom(r.Context())
		s, err := store.Get(r.Context(), p.MerchantID, p.Livemode(), r.PathValue("id"))
		if err != nil {
			writeCheckoutError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, renderCheckoutSession(s, publicURL))
	}
}

func handleListCheckoutSessions(store checkoutStore, publicURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params, apiErr := parseListParams(r)
		if apiErr != nil {
			writeError(w, http.StatusBadRequest, *apiErr)
			return
		}
		p := principalFrom(r.Context())
		page, hasMore, err := store.List(r.Context(), p.MerchantID, p.Livemode(), params)
		if errors.Is(err, checkout.ErrNotFound) {
			id, _ := params.Cursor()
			e := noSuchCheckoutSession(id)
			e.Param = cursorParam(params)
			writeError(w, http.StatusNotFound, e)
			return
		}
		if err != nil {
			writeCheckoutError(w, r, err)
			return
		}
		out := listJSON[checkoutSessionJSON]{Object: "list", URL: "/v1/checkout/sessions", HasMore: hasMore, Data: []checkoutSessionJSON{}}
		for _, s := range page {
			out.Data = append(out.Data, renderCheckoutSession(s, publicURL))
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func handleListCheckoutLineItems(store checkoutStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := principalFrom(r.Context())
		s, err := store.Get(r.Context(), p.MerchantID, p.Livemode(), r.PathValue("id"))
		if err != nil {
			writeCheckoutError(w, r, err)
			return
		}
		out := listJSON[lineItemJSON]{Object: "list", URL: "/v1/checkout/sessions/" + s.ID + "/line_items", Data: []lineItemJSON{}}
		for i, li := range s.LineItems {
			out.Data = append(out.Data, lineItemJSON{
				ID: fmt.Sprintf("li_%s_%d", strings.TrimPrefix(strings.TrimPrefix(s.ID, "cs_test_"), "cs_live_"), i), Object: "item",
				AmountTotal: li.AmountTotal(), Currency: li.Currency, Description: li.Name, Quantity: li.Quantity, UnitAmount: li.UnitAmount,
			})
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func handleExpireCheckoutSession(store checkoutStore, publicURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, apiErr := parseForm(w, r); apiErr != nil {
			writeError(w, http.StatusBadRequest, *apiErr)
			return
		}
		p := principalFrom(r.Context())
		s, err := store.Expire(r.Context(), p.MerchantID, p.Livemode(), r.PathValue("id"))
		if err != nil {
			writeCheckoutError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, renderCheckoutSession(s, publicURL))
	}
}

func noSuchCheckoutSession(id string) apiError {
	return apiError{Type: "invalid_request_error", Code: "resource_missing", Param: "session",
		Message: "No such checkout.session: '" + id + "'"}
}

func writeCheckoutError(w http.ResponseWriter, r *http.Request, err error) {
	var (
		ve *checkout.ValidationError
		se *checkout.StateError
		me *metadata.Error
	)
	switch {
	case errors.As(err, &ve):
		writeError(w, http.StatusBadRequest, apiError{Type: "invalid_request_error", Code: ve.Code, Param: ve.Param, Message: ve.Message})
	case errors.As(err, &me):
		writeError(w, http.StatusBadRequest, apiError{Type: "invalid_request_error", Code: "parameter_invalid_metadata", Param: me.Param, Message: me.Message})
	case errors.As(err, &se):
		writeError(w, http.StatusBadRequest, apiError{Type: "invalid_request_error", Code: "checkout_session_unexpected_state", Message: se.Message})
	case errors.Is(err, checkout.ErrNotFound):
		writeError(w, http.StatusNotFound, noSuchCheckoutSession(r.PathValue("id")))
	default:
		writeInternalError(w, r, "checkout session", err)
	}
}
