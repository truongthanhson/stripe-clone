package main

import (
	"net/http"
	"strconv"

	"github.com/truongthanhson/stripe-clone/internal/paymentintent"
)

const (
	defaultListLimit = 10
	maxListLimit     = 100
)

// parseListParams reads limit, starting_after and ending_before from the query string, as Stripe's list endpoints take.
func parseListParams(r *http.Request) (paymentintent.ListParams, *apiError) {
	q := r.URL.Query() // unknown names and the limit's range were already checked against openapi.yaml
	p := paymentintent.ListParams{
		Limit:         defaultListLimit,
		StartingAfter: q.Get("starting_after"),
		EndingBefore:  q.Get("ending_before"),
	}
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 || n > maxListLimit {
			return p, &apiError{Type: "invalid_request_error", Code: "parameter_invalid_integer", Param: "limit",
				Message: "Invalid limit: must be an integer between 1 and 100."}
		}
		p.Limit = n
	}
	if p.StartingAfter != "" && p.EndingBefore != "" {
		return p, &apiError{Type: "invalid_request_error", Code: "parameter_invalid", Param: "ending_before",
			Message: "You may only specify one of starting_after and ending_before."}
	}
	return p, nil
}
