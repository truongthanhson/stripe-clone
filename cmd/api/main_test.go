package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeDB struct{ err error }

func (f fakeDB) Ping(context.Context) error { return f.err }

func TestHealthz(t *testing.T) {
	tests := []struct {
		name     string
		db       fakeDB
		wantCode int
		wantBody string
	}{
		{"database up", fakeDB{}, http.StatusOK, "{\"database\":\"up\",\"status\":\"ok\"}\n"},
		{"database down", fakeDB{err: errors.New("connection refused")}, http.StatusServiceUnavailable, "{\"database\":\"down\",\"status\":\"unavailable\"}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)

			newRouter(tt.db).ServeHTTP(rec, req)

			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			if got := rec.Body.String(); got != tt.wantBody {
				t.Fatalf("body = %q, want %q", got, tt.wantBody)
			}
		})
	}
}
