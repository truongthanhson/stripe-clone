package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

type listResp struct {
	Object  string
	URL     string
	HasMore bool `json:"has_more"`
	Data    []struct {
		ID       string
		Created  int64
		Metadata map[string]string
	}
	Error *struct{ Code, Param string }
}

func (e e2e) list(t *testing.T, key, query string) (int, listResp) {
	t.Helper()
	code, raw := e.do(t, "GET", "/v1/payment_intents?"+query, key, nil)
	b, _ := json.Marshal(raw)
	var v listResp
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return code, v
}

func TestE2EList(t *testing.T) {
	e := newE2E(t)
	const total = 25
	var created []string // oldest first
	for i := range total {
		pi := e.create(t, url.Values{"amount": {fmt.Sprint(1000 + i)}})
		created = append(created, pi.ID)
	}
	// Noise that must never show up: live mode and another merchant.
	e.call(t, "/v1/payment_intents", e.liveKey, url.Values{"amount": {"2000"}, "currency": {"usd"}}, nil)
	e.call(t, "/v1/payment_intents", e.otherMerchantKey, url.Values{"amount": {"2000"}, "currency": {"usd"}}, nil)

	// Same second for every row: the id tie-break is what keeps paging exact.
	if _, err := e.pool.Exec(context.Background(), `UPDATE payment_intents SET created_at = '2026-01-01T00:00:00Z'`); err != nil {
		t.Fatal(err)
	}

	walk := func(t *testing.T, limit int) []string {
		var seen []string
		query := fmt.Sprintf("limit=%d", limit)
		for pages := 0; ; pages++ {
			if pages > total {
				t.Fatal("paging does not terminate")
			}
			code, page := e.list(t, e.testKey, query)
			if code != 200 || page.Object != "list" || page.URL != "/v1/payment_intents" {
				t.Fatalf("list: %d %+v", code, page)
			}
			for _, d := range page.Data {
				seen = append(seen, d.ID)
			}
			if !page.HasMore {
				return seen
			}
			query = fmt.Sprintf("limit=%d&starting_after=%s", limit, page.Data[len(page.Data)-1].ID)
		}
	}

	t.Run("starting_after walks every item exactly once, newest first", func(t *testing.T) {
		for _, limit := range []int{1, 7, 10, 25, 100} {
			seen := walk(t, limit)
			if len(seen) != total {
				t.Fatalf("limit %d: saw %d items, want %d", limit, len(seen), total)
			}
			unique := map[string]bool{}
			for _, id := range seen {
				unique[id] = true
			}
			if len(unique) != total {
				t.Fatalf("limit %d: duplicates in %v", limit, seen)
			}
		}
	})

	t.Run("ending_before pages back to the newer items", func(t *testing.T) {
		all := walk(t, 100)
		code, page := e.list(t, e.testKey, "limit=3&ending_before="+all[10])
		if code != 200 || !page.HasMore || len(page.Data) != 3 {
			t.Fatalf("got %d %+v", code, page)
		}
		for i, d := range page.Data {
			if d.ID != all[7+i] {
				t.Fatalf("item %d = %s, want %s", i, d.ID, all[7+i])
			}
		}
		_, first := e.list(t, e.testKey, "ending_before="+all[2])
		if first.HasMore || len(first.Data) != 2 {
			t.Fatalf("near the start: %+v", first)
		}
	})

	t.Run("default limit is 10", func(t *testing.T) {
		_, page := e.list(t, e.testKey, "")
		if len(page.Data) != 10 || !page.HasMore {
			t.Fatalf("got %d items, has_more %v", len(page.Data), page.HasMore)
		}
	})

	t.Run("errors", func(t *testing.T) {
		_, live := e.list(t, e.liveKey, "")
		if len(live.Data) != 1 {
			t.Fatalf("live list has %d items, want 1", len(live.Data))
		}
		cases := []struct {
			query, code, param string
			status             int
		}{
			{"limit=0", "parameter_invalid_integer", "limit", 400},
			{"limit=101", "parameter_invalid_integer", "limit", 400},
			{"limit=ten", "parameter_invalid_integer", "limit", 400},
			{"status=succeeded", "parameter_unknown", "status", 400},
			{"starting_after=" + created[0] + "&ending_before=" + created[1], "parameter_invalid", "ending_before", 400},
			{"starting_after=pi_missing", "resource_missing", "starting_after", 404},
			{"starting_after=" + live.Data[0].ID, "resource_missing", "starting_after", 404},
		}
		for _, c := range cases {
			code, page := e.list(t, e.testKey, c.query)
			if code != c.status || page.Error == nil || page.Error.Code != c.code || page.Error.Param != c.param {
				t.Errorf("%s: got %d %+v, want %d %s/%s", c.query, code, page.Error, c.status, c.code, c.param)
			}
		}
	})
}

func TestE2EMetadata(t *testing.T) {
	e := newE2E(t)
	update := func(t *testing.T, id string, form url.Values) (int, map[string]any) {
		t.Helper()
		rec := e.post(t, "/v1/payment_intents/"+id, e.testKey, form, nil)
		var v map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &v)
		return rec.Code, v
	}

	code, created := e.do(t, "POST", "/v1/payment_intents", e.testKey,
		url.Values{"amount": {"2000"}, "currency": {"usd"}, "metadata[order_id]": {"6735"}, "metadata[channel]": {"web"}})
	if code != 200 {
		t.Fatalf("create: %d %v", code, created)
	}
	id := created["id"].(string)
	wantMeta := func(t *testing.T, body map[string]any, want map[string]any) {
		t.Helper()
		got, _ := body["metadata"].(map[string]any)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("metadata = %v, want %v", got, want)
		}
	}
	wantMeta(t, created, map[string]any{"order_id": "6735", "channel": "web"})

	code, body := update(t, id, url.Values{"metadata[channel]": {""}, "metadata[sku]": {"A1"}, "description": {"Blue shirt"}})
	if code != 200 || body["description"] != "Blue shirt" {
		t.Fatalf("update: %d %v", code, body)
	}
	wantMeta(t, body, map[string]any{"order_id": "6735", "sku": "A1"})

	code, body = update(t, id, url.Values{"metadata": {""}})
	if code != 200 {
		t.Fatalf("clear: %d %v", code, body)
	}
	wantMeta(t, body, map[string]any{})
	if body["description"] != "Blue shirt" {
		t.Fatal("clearing metadata changed the description")
	}

	code, body = update(t, id, url.Values{"metadata[" + strings.Repeat("k", 41) + "]": {"v"}})
	if code != 400 || errorOf(body)["code"] != "parameter_invalid_metadata" {
		t.Fatalf("long key: %d %v", code, body)
	}
	if code, _ := update(t, "pi_missing", url.Values{"description": {"x"}}); code != 404 {
		t.Fatalf("update missing: %d", code)
	}
	if code, body := update(t, id, url.Values{"amount": {"1"}}); code != 400 || errorOf(body)["code"] != "parameter_unknown" {
		t.Fatalf("amount is not updatable: %d %v", code, body)
	}

	_, fetched := e.do(t, "GET", "/v1/payment_intents/"+id, e.testKey, nil)
	wantMeta(t, fetched, map[string]any{})
}
