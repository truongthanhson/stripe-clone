package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3filter"
)

// TestRoutesMatchSpec fails when a route is served but not documented, or documented but not served.
func TestRoutesMatchSpec(t *testing.T) {
	var served []string
	for _, rt := range routes(app{}) {
		served = append(served, rt.pattern)
	}
	var documented []string
	for path, item := range loadSpec().doc.Paths.Map() {
		for method := range item.Operations() {
			documented = append(documented, method+" "+path)
		}
	}
	slices.Sort(served)
	slices.Sort(documented)
	if !slices.Equal(served, documented) {
		t.Fatalf("routes and openapi.yaml differ\nserved:     %v\ndocumented: %v", served, documented)
	}
}

func TestRequestValidation(t *testing.T) {
	router := newRouter(app{keys: testKeys, intents: fakeIntents{}})
	tests := []struct {
		name, method, path, body string
		wantCode, wantParam      string
	}{
		{"unknown body param", "POST", "/v1/payment_intents", "amount=2000&currency=usd&colour=red", "parameter_unknown", "colour"},
		{"unknown query param", "GET", "/v1/payment_intents?status=succeeded", "", "parameter_unknown", "status"},
		{"missing required", "POST", "/v1/payment_intents", "currency=usd", "parameter_missing", "amount"},
		{"integer", "POST", "/v1/payment_intents", "amount=twenty&currency=usd", "parameter_invalid_integer", "amount"},
		{"boolean", "POST", "/v1/payment_intents", "amount=2000&currency=usd&confirm=yes", "parameter_invalid_boolean", "confirm"},
		{"enum", "POST", "/v1/payment_intents", "amount=2000&currency=usd&capture_method=later", "parameter_invalid_string", "capture_method"},
		{"max length", "POST", "/v1/payment_intents", "amount=2000&currency=usd&description=" + strings.Repeat("x", 1001), "parameter_invalid_string", "description"},
		{"query range", "GET", "/v1/payment_intents?limit=500", "", "parameter_invalid_integer", "limit"},
		{"hash on a scalar", "POST", "/v1/payment_intents", "amount=2000&currency=usd&description[x]=1", "parameter_unknown", "description[x]"},
		{"scalar on a hash", "POST", "/v1/payment_intents", "amount=2000&currency=usd&metadata=x", "parameter_invalid", "metadata"},
		{"operation without a body", "POST", "/v1/payment_intents/pi_1/capture", "amount=1", "parameter_unknown", "amount"},
		{"enum on another endpoint", "POST", "/v1/payment_intents/pi_1/cancel", "cancellation_reason=bored", "parameter_invalid_string", "cancellation_reason"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			if tt.body != "" {
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			}
			req.Header.Set("Authorization", "Bearer sk_test_good")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			e := errorOf(decode(t, rec.Body.Bytes()))
			if rec.Code != 400 || e["code"] != tt.wantCode || e["param"] != tt.wantParam {
				t.Fatalf("got %d %v, want 400 %s/%s", rec.Code, e, tt.wantCode, tt.wantParam)
			}
		})
	}

	t.Run("metadata hash and clear are accepted", func(t *testing.T) {
		code, body := do(t, router, "POST", "/v1/payment_intents", "sk_test_good",
			url.Values{"amount": {"2000"}, "currency": {"usd"}, "metadata[order]": {"1"}, "metadata[x]": {""}})
		if code != 200 {
			t.Fatalf("got %d %v", code, body)
		}
	})
}

func TestSpecIsServed(t *testing.T) {
	rec := httptest.NewRecorder()
	newRouter(app{}).ServeHTTP(rec, httptest.NewRequest("GET", "/openapi.yaml", nil))
	if rec.Code != 200 || !strings.HasPrefix(rec.Body.String(), "openapi: 3.0.3") {
		t.Fatalf("got %d %.40q", rec.Code, rec.Body)
	}
}

// withContractCheck validates every response the wrapped handler writes against openapi.yaml.
// The e2e tests run through it, so each of them is also a contract test: a field added in code but
// not in the spec, a wrong type, or an undocumented status fails the test that produced it.
func withContractCheck(t *testing.T, next http.Handler) http.Handler {
	spec := loadSpec()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)

		if route, pathParams, err := spec.router.FindRoute(r); err == nil {
			input := &openapi3filter.ResponseValidationInput{
				RequestValidationInput: &openapi3filter.RequestValidationInput{
					Request: r, PathParams: pathParams, Route: route,
					Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
				},
				Status: rec.Code,
				Header: rec.Header(),
				Body:   io.NopCloser(bytes.NewReader(rec.Body.Bytes())),
			}
			if err := openapi3filter.ValidateResponse(context.Background(), input); err != nil {
				t.Errorf("%s %s -> %d does not match openapi.yaml: %v\nbody: %s", r.Method, r.URL.Path, rec.Code, err, rec.Body)
			}
		}

		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	})
}
