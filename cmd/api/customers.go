package main

import (
	"context"
	"errors"
	"net/http"

	"github.com/truongthanhson/stripe-clone/internal/customer"
	"github.com/truongthanhson/stripe-clone/internal/listing"
	"github.com/truongthanhson/stripe-clone/internal/metadata"
)

type customerStore interface {
	Create(ctx context.Context, merchantID int64, livemode bool, p customer.Params) (customer.Customer, error)
	Get(ctx context.Context, merchantID int64, livemode bool, id string) (customer.Customer, error)
	Update(ctx context.Context, merchantID int64, livemode bool, id string, p customer.Params) (customer.Customer, error)
	Delete(ctx context.Context, merchantID int64, livemode bool, id string) error
	List(ctx context.Context, merchantID int64, livemode bool, email string, p listing.Params) ([]customer.Customer, bool, error)
}

type customerJSON struct {
	ID          string            `json:"id"`
	Object      string            `json:"object"`
	Created     int64             `json:"created"`
	Description *string           `json:"description"`
	Email       *string           `json:"email"`
	Livemode    bool              `json:"livemode"`
	Metadata    map[string]string `json:"metadata"`
	Name        *string           `json:"name"`
}

// deletedJSON is what Stripe returns for a deleted object: just enough to know it is gone.
type deletedJSON struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Deleted bool   `json:"deleted"`
}

func renderCustomer(c customer.Customer) any {
	if c.Deleted {
		return deletedJSON{ID: c.ID, Object: "customer", Deleted: true}
	}
	md := c.Metadata
	if md == nil {
		md = map[string]string{}
	}
	return customerJSON{
		ID: c.ID, Object: "customer", Created: c.Created.Unix(), Description: c.Description, Email: c.Email,
		Livemode: c.Livemode, Metadata: md, Name: c.Name,
	}
}

func customerParams(w http.ResponseWriter, r *http.Request) (customer.Params, *apiError) {
	form, apiErr := parseForm(w, r)
	if apiErr != nil {
		return customer.Params{}, apiErr
	}
	p := customer.Params{
		Email:       optionalString(form, "email"),
		Name:        optionalString(form, "name"),
		Description: optionalString(form, "description"),
	}
	if hasMetadata(form) {
		u := parseMetadata(form)
		p.Metadata = &u
	}
	return p, nil
}

func handleCreateCustomer(store customerStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params, apiErr := customerParams(w, r)
		if apiErr != nil {
			writeError(w, http.StatusBadRequest, *apiErr)
			return
		}
		p := principalFrom(r.Context())
		c, err := store.Create(r.Context(), p.MerchantID, p.Livemode(), params)
		if err != nil {
			writeCustomerError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, renderCustomer(c))
	}
}

func handleGetCustomer(store customerStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := principalFrom(r.Context())
		c, err := store.Get(r.Context(), p.MerchantID, p.Livemode(), r.PathValue("id"))
		if err != nil {
			writeCustomerError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, renderCustomer(c))
	}
}

func handleUpdateCustomer(store customerStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params, apiErr := customerParams(w, r)
		if apiErr != nil {
			writeError(w, http.StatusBadRequest, *apiErr)
			return
		}
		p := principalFrom(r.Context())
		c, err := store.Update(r.Context(), p.MerchantID, p.Livemode(), r.PathValue("id"), params)
		if err != nil {
			writeCustomerError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, renderCustomer(c))
	}
}

func handleDeleteCustomer(store customerStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := principalFrom(r.Context())
		id := r.PathValue("id")
		if err := store.Delete(r.Context(), p.MerchantID, p.Livemode(), id); err != nil {
			writeCustomerError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, deletedJSON{ID: id, Object: "customer", Deleted: true})
	}
}

func handleListCustomers(store customerStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params, apiErr := parseListParams(r)
		if apiErr != nil {
			writeError(w, http.StatusBadRequest, *apiErr)
			return
		}
		p := principalFrom(r.Context())
		page, hasMore, err := store.List(r.Context(), p.MerchantID, p.Livemode(), r.URL.Query().Get("email"), params)
		if errors.Is(err, customer.ErrNotFound) {
			id, _ := params.Cursor()
			e := noSuchCustomer(id)
			e.Param = cursorParam(params)
			writeError(w, http.StatusNotFound, e)
			return
		}
		if err != nil {
			writeCustomerError(w, r, err)
			return
		}
		out := listJSON[any]{Object: "list", URL: "/v1/customers", HasMore: hasMore, Data: []any{}}
		for _, c := range page {
			out.Data = append(out.Data, renderCustomer(c))
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func noSuchCustomer(id string) apiError {
	return apiError{Type: "invalid_request_error", Code: "resource_missing", Param: "id", Message: "No such customer: '" + id + "'"}
}

func writeCustomerError(w http.ResponseWriter, r *http.Request, err error) {
	var (
		ve *customer.ValidationError
		me *metadata.Error
	)
	switch {
	case errors.As(err, &ve):
		writeError(w, http.StatusBadRequest, apiError{Type: "invalid_request_error", Code: "parameter_invalid_string", Param: ve.Param, Message: ve.Message})
	case errors.As(err, &me):
		writeError(w, http.StatusBadRequest, apiError{Type: "invalid_request_error", Code: "parameter_invalid_metadata", Param: me.Param, Message: me.Message})
	case errors.Is(err, customer.ErrNotFound):
		writeError(w, http.StatusNotFound, noSuchCustomer(r.PathValue("id")))
	default:
		writeInternalError(w, r, "customer", err)
	}
}
