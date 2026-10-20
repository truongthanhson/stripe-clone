package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestLogging(t *testing.T) {
	tests := []struct {
		name     string
		incoming string
		wantSame bool
	}{
		{"generates an ID when none is sent", "", false},
		{"keeps a valid incoming ID", "abc-123", true},
		{"replaces an invalid incoming ID", "bad id\nwith newline", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))

			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				loggerFrom(r.Context()).Info("inside handler")
				w.WriteHeader(http.StatusTeapot)
			})

			req := httptest.NewRequest(http.MethodGet, "/brew", nil)
			if tt.incoming != "" {
				req.Header.Set(requestIDHeader, tt.incoming)
			}
			rec := httptest.NewRecorder()
			withRequestLogging(logger, next).ServeHTTP(rec, req)

			id := rec.Header().Get(requestIDHeader)
			if id == "" {
				t.Fatal("response has no request ID header")
			}
			if (id == tt.incoming) != tt.wantSame {
				t.Fatalf("request ID = %q, incoming %q, wantSame %v", id, tt.incoming, tt.wantSame)
			}

			lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
			if len(lines) != 2 {
				t.Fatalf("got %d log lines, want 2:\n%s", len(lines), logs.String())
			}
			for _, line := range lines {
				var entry map[string]any
				if err := json.Unmarshal([]byte(line), &entry); err != nil {
					t.Fatal(err)
				}
				if entry["request_id"] != id {
					t.Fatalf("log line request_id = %v, want %q: %s", entry["request_id"], id, line)
				}
			}

			var access map[string]any
			_ = json.Unmarshal([]byte(lines[1]), &access)
			if access["msg"] != "request" || access["method"] != "GET" || access["path"] != "/brew" || access["status"] != float64(http.StatusTeapot) {
				t.Fatalf("unexpected access log line: %s", lines[1])
			}
			if _, ok := access["duration_ms"].(float64); !ok {
				t.Fatalf("access log has no duration_ms: %s", lines[1])
			}
		})
	}
}

func TestRequestLoggingDefaultStatus(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	withRequestLogging(logger, next).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	var access map[string]any
	if err := json.Unmarshal(logs.Bytes(), &access); err != nil {
		t.Fatal(err)
	}
	if access["status"] != float64(http.StatusOK) {
		t.Fatalf("status = %v, want 200", access["status"])
	}
}
