package main

import (
	"encoding/json"
	"net/http"
)

// apiError is the one error shape every endpoint returns, modeled on Stripe's.
type apiError struct {
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
	Param   string `json:"param,omitempty"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, e apiError) {
	writeJSON(w, status, map[string]apiError{"error": e})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

var errInternal = apiError{
	Type:    "api_error",
	Message: "Something went wrong on our end.",
}

// writeInternalError logs the cause, which stays private, and returns a generic 500.
func writeInternalError(w http.ResponseWriter, r *http.Request, msg string, err error) {
	loggerFrom(r.Context()).Error(msg, "error", err)
	writeError(w, http.StatusInternalServerError, errInternal)
}
