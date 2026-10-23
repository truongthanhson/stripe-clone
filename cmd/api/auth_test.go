package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/truongthanhson/stripe-clone/internal/apikey"
)

type fakeKeys map[string]apikey.Principal

func (f fakeKeys) Lookup(_ context.Context, secret string) (apikey.Principal, error) {
	if secret == "sk_test_dberror" {
		return apikey.Principal{}, errors.New("connection refused")
	}
	p, ok := f[secret]
	if !ok {
		return apikey.Principal{}, apikey.ErrNotFound
	}
	return p, nil
}

func TestAccountAuth(t *testing.T) {
	keys := fakeKeys{
		"sk_test_good": {MerchantID: 7, Mode: apikey.ModeTest},
		"sk_live_good": {MerchantID: 7, Mode: apikey.ModeLive},
	}
	router := newRouter(app{db: fakeDB{}, keys: keys})

	tests := []struct {
		name         string
		auth         string
		wantStatus   int
		wantCode     string
		wantLivemode bool
	}{
		{"no header", "", http.StatusUnauthorized, "api_key_missing", false},
		{"wrong scheme", "Basic sk_test_good", http.StatusUnauthorized, "api_key_missing", false},
		{"not a secret key", "Bearer pk_test_good", http.StatusUnauthorized, "api_key_invalid", false},
		{"unknown key", "Bearer sk_test_nope", http.StatusUnauthorized, "api_key_invalid", false},
		{"lookup fails", "Bearer sk_test_dberror", http.StatusInternalServerError, "", false},
		{"test key", "Bearer sk_test_good", http.StatusOK, "", false},
		{"live key", "Bearer sk_live_good", http.StatusOK, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/account", nil)
			if tt.auth != "" {
				req.Header.Set("Authorization", tt.auth)
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body)
			}

			if rec.Code != http.StatusOK {
				var body struct {
					Error apiError `json:"error"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if body.Error.Type == "" || body.Error.Message == "" || body.Error.Code != tt.wantCode {
					t.Fatalf("error body = %s, want code %q", rec.Body, tt.wantCode)
				}
				return
			}

			var account struct {
				Object   string `json:"object"`
				ID       int64  `json:"id"`
				Livemode bool   `json:"livemode"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &account); err != nil {
				t.Fatal(err)
			}
			if account.Object != "account" || account.ID != 7 || account.Livemode != tt.wantLivemode {
				t.Fatalf("account = %+v, want id 7 livemode %v", account, tt.wantLivemode)
			}
		})
	}
}

func TestHealthzNeedsNoKey(t *testing.T) {
	rec := httptest.NewRecorder()
	newRouter(app{db: fakeDB{}, keys: fakeKeys{}}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}
