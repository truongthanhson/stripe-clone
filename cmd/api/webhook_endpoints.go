package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/truongthanhson/stripe-clone/internal/listing"
	"github.com/truongthanhson/stripe-clone/internal/webhook"
)

type endpointStore interface {
	Create(ctx context.Context, merchantID int64, livemode bool, p webhook.Params) (webhook.Endpoint, error)
	Get(ctx context.Context, merchantID int64, livemode bool, id string) (webhook.Endpoint, error)
	Update(ctx context.Context, merchantID int64, livemode bool, id string, p webhook.Params) (webhook.Endpoint, error)
	Delete(ctx context.Context, merchantID int64, livemode bool, id string) error
	List(ctx context.Context, merchantID int64, livemode bool, p listing.Params) ([]webhook.Endpoint, bool, error)
}

// webhookEndpointJSON follows Stripe's WebhookEndpoint. secret is only ever sent in the response to
// the create request.
type webhookEndpointJSON struct {
	ID            string   `json:"id"`
	Object        string   `json:"object"`
	Created       int64    `json:"created"`
	Description   *string  `json:"description"`
	EnabledEvents []string `json:"enabled_events"`
	Livemode      bool     `json:"livemode"`
	Secret        string   `json:"secret,omitempty"`
	Status        string   `json:"status"`
	URL           string   `json:"url"`
}

func renderEndpoint(ep webhook.Endpoint, withSecret bool) webhookEndpointJSON {
	out := webhookEndpointJSON{ID: ep.ID, Object: "webhook_endpoint", Created: ep.Created.Unix(), Description: ep.Description,
		EnabledEvents: ep.EnabledEvents, Livemode: ep.Livemode, Status: ep.Status, URL: ep.URL}
	if withSecret {
		out.Secret = ep.Secret
	}
	return out
}

var arrayItem = regexp.MustCompile(`^\[(\d*)\]$`)

// parseArray reads Stripe's array syntax: name[]=a&name[]=b, or name[0]=a&name[1]=b in index order.
func parseArray(form url.Values, name string) []string {
	type item struct {
		index  int
		values []string
	}
	var items []item
	for key, values := range form {
		rest, ok := strings.CutPrefix(key, name)
		if !ok {
			continue
		}
		m := arrayItem.FindStringSubmatch(rest)
		if m == nil {
			continue
		}
		i := -1
		if m[1] != "" {
			i, _ = strconv.Atoi(m[1])
		}
		items = append(items, item{i, values})
	}
	slices.SortFunc(items, func(a, b item) int { return a.index - b.index })
	var out []string
	for _, it := range items {
		for _, v := range it.values {
			if !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
	}
	return out
}

func endpointParams(w http.ResponseWriter, r *http.Request) (webhook.Params, *apiError) {
	form, apiErr := parseForm(w, r)
	if apiErr != nil {
		return webhook.Params{}, apiErr
	}
	p := webhook.Params{
		URL:           optionalString(form, "url"),
		Description:   optionalString(form, "description"),
		EnabledEvents: parseArray(form, "enabled_events"),
	}
	if form.Has("disabled") {
		disabled, apiErr := optionalBool(form, "disabled")
		if apiErr != nil {
			return webhook.Params{}, apiErr
		}
		p.Disabled = &disabled
	}
	return p, nil
}

func handleCreateEndpoint(store endpointStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params, apiErr := endpointParams(w, r)
		if apiErr != nil {
			writeError(w, http.StatusBadRequest, *apiErr)
			return
		}
		p := principalFrom(r.Context())
		ep, err := store.Create(r.Context(), p.MerchantID, p.Livemode(), params)
		if err != nil {
			writeEndpointError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, renderEndpoint(ep, true))
	}
}

func handleGetEndpoint(store endpointStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := principalFrom(r.Context())
		ep, err := store.Get(r.Context(), p.MerchantID, p.Livemode(), r.PathValue("id"))
		if err != nil {
			writeEndpointError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, renderEndpoint(ep, false))
	}
}

func handleUpdateEndpoint(store endpointStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params, apiErr := endpointParams(w, r)
		if apiErr != nil {
			writeError(w, http.StatusBadRequest, *apiErr)
			return
		}
		p := principalFrom(r.Context())
		ep, err := store.Update(r.Context(), p.MerchantID, p.Livemode(), r.PathValue("id"), params)
		if err != nil {
			writeEndpointError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, renderEndpoint(ep, false))
	}
}

func handleDeleteEndpoint(store endpointStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := principalFrom(r.Context())
		id := r.PathValue("id")
		if err := store.Delete(r.Context(), p.MerchantID, p.Livemode(), id); err != nil {
			writeEndpointError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, deletedJSON{ID: id, Object: "webhook_endpoint", Deleted: true})
	}
}

func handleListEndpoints(store endpointStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params, apiErr := parseListParams(r)
		if apiErr != nil {
			writeError(w, http.StatusBadRequest, *apiErr)
			return
		}
		p := principalFrom(r.Context())
		page, hasMore, err := store.List(r.Context(), p.MerchantID, p.Livemode(), params)
		if errors.Is(err, webhook.ErrNotFound) {
			id, _ := params.Cursor()
			e := noSuchEndpoint(id)
			e.Param = cursorParam(params)
			writeError(w, http.StatusNotFound, e)
			return
		}
		if err != nil {
			writeEndpointError(w, r, err)
			return
		}
		out := listJSON[webhookEndpointJSON]{Object: "list", URL: "/v1/webhook_endpoints", HasMore: hasMore, Data: []webhookEndpointJSON{}}
		for _, ep := range page {
			out.Data = append(out.Data, renderEndpoint(ep, false))
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func noSuchEndpoint(id string) apiError {
	return apiError{Type: "invalid_request_error", Code: "resource_missing", Param: "id", Message: "No such webhook endpoint: '" + id + "'"}
}

func writeEndpointError(w http.ResponseWriter, r *http.Request, err error) {
	var ve *webhook.ValidationError
	switch {
	case errors.As(err, &ve):
		writeError(w, http.StatusBadRequest, apiError{Type: "invalid_request_error", Code: ve.Code, Param: ve.Param, Message: ve.Message})
	case errors.Is(err, webhook.ErrNotFound):
		writeError(w, http.StatusNotFound, noSuchEndpoint(r.PathValue("id")))
	default:
		writeInternalError(w, r, "webhook endpoint", err)
	}
}
