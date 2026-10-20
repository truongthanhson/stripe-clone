package main

import (
	"encoding/json"
	"net/http"
)

// apiError is the one error shape every endpoint returns, modeled on Stripe's.
type apiError struct {
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, e apiError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]apiError{"error": e})
}
