package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"

	"github.com/truongthanhson/stripe-clone/openapi"
)

// apiSpec is the parsed OpenAPI document plus a router that finds the operation for a request.
type apiSpec struct {
	doc    *openapi3.T
	router routers.Router
}

// loadSpec parses the embedded document once. It is embedded at build time, so failing here is a
// programming error that every test run catches.
var loadSpec = sync.OnceValue(func() *apiSpec {
	doc, err := openapi3.NewLoader().LoadFromData(openapi.Spec)
	if err != nil {
		panic(fmt.Sprintf("openapi.yaml: %v", err))
	}
	if err := doc.Validate(context.Background()); err != nil {
		panic(fmt.Sprintf("openapi.yaml is invalid: %v", err))
	}
	// Match on path only; the servers list is documentation and must not tie routing to a host.
	routable := *doc
	routable.Servers = nil
	router, err := legacy.NewRouter(&routable)
	if err != nil {
		panic(fmt.Sprintf("openapi.yaml routes: %v", err))
	}
	return &apiSpec{doc: &routable, router: router}
})

func handleSpec(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write(openapi.Spec)
}

// withRequestValidation checks every request against its operation in the spec before the handler
// runs: unknown query or body parameters, missing required ones, and values of the wrong type or
// outside an enum get a Stripe-style 400 naming the parameter. Handlers never see a parameter
// the spec does not declare, so the spec cannot silently drift from the code.
func withRequestValidation(spec *apiSpec, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route, _, err := spec.router.FindRoute(r)
		if err != nil {
			next.ServeHTTP(w, r) // the mux answers 404 or 405
			return
		}
		if apiErr := validateQuery(route, r.URL.Query()); apiErr != nil {
			writeError(w, http.StatusBadRequest, *apiErr)
			return
		}
		if r.Method == http.MethodPost && isForm(r) {
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
			if err != nil {
				writeError(w, http.StatusBadRequest, apiError{Type: "invalid_request_error", Code: "request_too_large",
					Message: "The request body is too large."})
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			form, err := url.ParseQuery(string(body))
			if err != nil {
				writeError(w, http.StatusBadRequest, apiError{Type: "invalid_request_error", Code: "parameter_invalid",
					Message: "The request body could not be parsed."})
				return
			}
			if apiErr := validateForm(route.Operation, form); apiErr != nil {
				writeError(w, http.StatusBadRequest, *apiErr)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func isForm(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		return true
	}
	mt, _, _ := mime.ParseMediaType(ct)
	return mt == "application/x-www-form-urlencoded"
}

func validateQuery(route *routers.Route, query url.Values) *apiError {
	declared := map[string]*openapi3.Schema{}
	for _, params := range []openapi3.Parameters{route.PathItem.Parameters, route.Operation.Parameters} {
		for _, p := range params {
			if p.Value.In == openapi3.ParameterInQuery {
				declared[p.Value.Name] = p.Value.Schema.Value
			}
		}
	}
	for _, name := range sortedKeys(query) {
		schema, ok := declared[name]
		if !ok {
			return unknownParam(name)
		}
		if apiErr := checkValue(name, query.Get(name), schema); apiErr != nil {
			return apiErr
		}
	}
	return nil
}

func validateForm(op *openapi3.Operation, form url.Values) *apiError {
	var schema *openapi3.Schema
	if op.RequestBody != nil {
		if mt := op.RequestBody.Value.Content.Get("application/x-www-form-urlencoded"); mt != nil {
			schema = mt.Schema.Value
		}
	}
	for _, name := range sortedKeys(form) {
		base, isHash := hashBase(name)
		if schema == nil || schema.Properties[base] == nil {
			return unknownParam(name)
		}
		prop := schema.Properties[base].Value
		isObject := prop.Type.Is(openapi3.TypeObject)
		isArray := prop.Type.Is(openapi3.TypeArray)
		switch {
		case isArray && !isHash:
			return &apiError{Type: "invalid_request_error", Code: "parameter_invalid", Param: name,
				Message: "Invalid " + name + ": send it as " + name + "[]=value, once per value."}
		case isArray:
			if !arrayIndex.MatchString(name[len(base):]) {
				return unknownParam(name)
			}
			for _, v := range form[name] {
				if apiErr := checkValue(base, v, prop.Items.Value); apiErr != nil {
					return apiErr
				}
			}
		case isHash && !isObject:
			return unknownParam(name)
		case isHash:
			// Keys and values of hashes (metadata) are checked by the domain, which names the key.
		case isObject && form.Get(name) != "":
			return &apiError{Type: "invalid_request_error", Code: "parameter_invalid", Param: name,
				Message: "Invalid " + name + ": send it as " + name + "[key]=value, or " + name + "= to clear it."}
		case isObject:
		default:
			if apiErr := checkValue(name, form.Get(name), prop); apiErr != nil {
				return apiErr
			}
		}
	}
	if schema != nil {
		for _, name := range schema.Required {
			if form.Get(name) == "" {
				return &apiError{Type: "invalid_request_error", Code: "parameter_missing", Param: name,
					Message: "Missing required param: " + name + "."}
			}
		}
	}
	return nil
}

// arrayIndex matches the part after an array parameter's name: "[]" or "[0]".
var arrayIndex = regexp.MustCompile(`^\[\d*\]$`)

// hashBase splits "metadata[order_id]" into ("metadata", true).
func hashBase(name string) (string, bool) {
	base, rest, found := strings.Cut(name, "[")
	if !found || !strings.HasSuffix(rest, "]") {
		return name, false
	}
	return base, true
}

func checkValue(name, value string, schema *openapi3.Schema) *apiError {
	invalid := func(code, msg string) *apiError {
		return &apiError{Type: "invalid_request_error", Code: code, Param: name, Message: msg}
	}
	switch {
	case schema.Type.Is(openapi3.TypeInteger):
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return invalid("parameter_invalid_integer", "Invalid integer: "+value)
		}
		if (schema.Min != nil && float64(n) < *schema.Min) || (schema.Max != nil && float64(n) > *schema.Max) {
			return invalid("parameter_invalid_integer", fmt.Sprintf("Invalid %s: must be between %v and %v.", name, deref(schema.Min), deref(schema.Max)))
		}
	case schema.Type.Is(openapi3.TypeBoolean):
		if value != "true" && value != "false" {
			return invalid("parameter_invalid_boolean", "Invalid boolean: "+value)
		}
	case schema.Type.Is(openapi3.TypeString):
		if len(schema.Enum) > 0 && !slices.Contains(schema.Enum, any(value)) {
			return invalid("parameter_invalid_string", fmt.Sprintf("Invalid %s: must be one of %v, got %q.", name, schema.Enum, value))
		}
		if schema.MaxLength != nil && uint64(len(value)) > *schema.MaxLength {
			return invalid("parameter_invalid_string", fmt.Sprintf("Invalid %s: must be at most %d characters.", name, *schema.MaxLength))
		}
	}
	return nil
}

func deref(f *float64) any {
	if f == nil {
		return "any"
	}
	return *f
}

func unknownParam(name string) *apiError {
	return &apiError{Type: "invalid_request_error", Code: "parameter_unknown", Param: name,
		Message: "Received unknown parameter: " + name}
}

func sortedKeys(v url.Values) []string {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
