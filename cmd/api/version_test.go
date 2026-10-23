package main

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/truongthanhson/stripe-clone/internal/apiversion"
)

func TestE2EAPIVersions(t *testing.T) {
	e := newE2E(t)
	form := url.Values{"amount": {"2000"}, "currency": {"usd"}}
	at := func(v string) map[string]string { return map[string]string{"Stripe-Version": v} }

	t.Run("new merchants get the latest version", func(t *testing.T) {
		rec := e.post(t, "/v1/payment_intents", e.testKey, form, nil)
		if rec.Header().Get("Stripe-Version") != apiversion.Latest() {
			t.Fatalf("Stripe-Version = %q", rec.Header().Get("Stripe-Version"))
		}
		if pi := decode(t, rec.Body.Bytes()); pi["capture_method"] != "automatic_async" {
			t.Fatalf("capture_method = %v", pi["capture_method"])
		}
	})

	t.Run("a request can ask for an older version", func(t *testing.T) {
		rec := e.post(t, "/v1/payment_intents", e.testKey, form, at(apiversion.V2026_10_01))
		pi := decode(t, rec.Body.Bytes())
		if rec.Header().Get("Stripe-Version") != apiversion.V2026_10_01 || pi["capture_method"] != "automatic" {
			t.Fatalf("got %s %v", rec.Header().Get("Stripe-Version"), pi["capture_method"])
		}
		var stored string
		_ = e.pool.QueryRow(context.Background(), `SELECT capture_method FROM payment_intents WHERE id = $1`, pi["id"]).Scan(&stored)
		if stored != "automatic" {
			t.Fatalf("stored %q: the old default must apply, not just the old rendering", stored)
		}
	})

	t.Run("a newer object is rendered in the old shape for an old version", func(t *testing.T) {
		created := decode(t, e.post(t, "/v1/payment_intents", e.testKey, form, nil).Body.Bytes())
		path := "/v1/payment_intents/" + created["id"].(string)
		req := e.post(t, path, e.testKey, url.Values{"description": {"x"}}, at(apiversion.V2026_10_01))
		if got := decode(t, req.Body.Bytes())["capture_method"]; got != "automatic" {
			t.Fatalf("capture_method = %v for 2026-10-01", got)
		}
	})

	t.Run("a merchant pinned to an old version gets it without a header", func(t *testing.T) {
		var merchant int64
		_ = e.pool.QueryRow(context.Background(),
			`SELECT merchant_id FROM api_keys WHERE prefix = $1`, e.testKey[:12]).Scan(&merchant)
		if _, err := e.pool.Exec(context.Background(), `UPDATE merchants SET api_version = $2 WHERE id = $1`,
			merchant, apiversion.V2026_10_01); err != nil {
			t.Fatal(err)
		}
		defer e.pool.Exec(context.Background(), `UPDATE merchants SET api_version = $2 WHERE id = $1`, merchant, apiversion.Latest())

		_, list := e.do(t, "GET", "/v1/payment_intents?limit=100", e.testKey, nil)
		for _, item := range list["data"].([]any) {
			if cm := item.(map[string]any)["capture_method"]; cm == "automatic_async" {
				t.Fatal("list for a 2026-10-01 merchant contains automatic_async")
			}
		}
		rec := e.post(t, "/v1/payment_intents", e.testKey, form, at(apiversion.Latest()))
		if decode(t, rec.Body.Bytes())["capture_method"] != "automatic_async" {
			t.Fatal("the header must override the pinned version")
		}
	})

	t.Run("an idempotent replay is rendered for the retry's version", func(t *testing.T) {
		key := map[string]string{"Idempotency-Key": "versioned-1"}
		first := e.post(t, "/v1/payment_intents", e.testKey, form, key)
		key["Stripe-Version"] = apiversion.V2026_10_01
		replay := e.post(t, "/v1/payment_intents", e.testKey, form, key)
		a, b := decode(t, first.Body.Bytes()), decode(t, replay.Body.Bytes())
		if a["id"] != b["id"] || a["capture_method"] != "automatic_async" || b["capture_method"] != "automatic" {
			t.Fatalf("first %v %v, replay %v %v", a["id"], a["capture_method"], b["id"], b["capture_method"])
		}
	})

	t.Run("unknown versions are rejected", func(t *testing.T) {
		rec := e.post(t, "/v1/payment_intents", e.testKey, form, at("2020-01-01"))
		if rec.Code != http.StatusBadRequest || errorCode(t, rec.Body.Bytes()) != "api_version_invalid" {
			t.Fatalf("got %d %s", rec.Code, rec.Body)
		}
	})
}
