package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func TestE2EIdempotency(t *testing.T) {
	e := newE2E(t)
	form := url.Values{"amount": {"2000"}, "currency": {"usd"}}
	key := func(k string) map[string]string { return map[string]string{"Idempotency-Key": k} }
	idOf := func(t *testing.T, body []byte) string {
		t.Helper()
		var v struct{ ID string }
		if err := json.Unmarshal(body, &v); err != nil {
			t.Fatal(err)
		}
		return v.ID
	}

	t.Run("a retry replays the first response", func(t *testing.T) {
		first := e.post(t, "/v1/payment_intents", e.testKey, form, key("retry-1"))
		second := e.post(t, "/v1/payment_intents", e.testKey, form, key("retry-1"))
		if first.Code != 200 || second.Code != 200 {
			t.Fatalf("status %d then %d", first.Code, second.Code)
		}
		if first.Body.String() != second.Body.String() {
			t.Fatalf("replay differs:\n%s\n%s", first.Body, second.Body)
		}
		if first.Header().Get("Idempotent-Replayed") != "" || second.Header().Get("Idempotent-Replayed") != "true" {
			t.Fatal("Idempotent-Replayed header should be set on the replay only")
		}
		if n := e.count(t, `SELECT count(*) FROM payment_intents WHERE id = $1`, idOf(t, first.Body.Bytes())); n != 1 {
			t.Fatalf("%d rows", n)
		}
	})

	t.Run("the same key with different parameters is rejected", func(t *testing.T) {
		e.post(t, "/v1/payment_intents", e.testKey, form, key("mismatch-1"))
		rec := e.post(t, "/v1/payment_intents", e.testKey, url.Values{"amount": {"9999"}, "currency": {"usd"}}, key("mismatch-1"))
		if rec.Code != http.StatusBadRequest || errorCode(t, rec.Body.Bytes()) != "idempotency_key_in_use" {
			t.Fatalf("got %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("a request rejected for bad parameters can be fixed and retried with the same key", func(t *testing.T) {
		bad := e.post(t, "/v1/payment_intents", e.testKey, url.Values{"amount": {"1"}, "currency": {"usd"}}, key("fix-1"))
		if bad.Code != http.StatusBadRequest {
			t.Fatalf("bad request: %d", bad.Code)
		}
		if good := e.post(t, "/v1/payment_intents", e.testKey, form, key("fix-1")); good.Code != 200 {
			t.Fatalf("fixed request: %d %s", good.Code, good.Body)
		}
	})

	t.Run("test and live keys do not share idempotency keys", func(t *testing.T) {
		test := e.post(t, "/v1/payment_intents", e.testKey, form, key("mode-1"))
		live := e.post(t, "/v1/payment_intents", e.liveKey, form, key("mode-1"))
		if test.Code != 200 || live.Code != 200 || idOf(t, test.Body.Bytes()) == idOf(t, live.Body.Bytes()) {
			t.Fatalf("test %d live %d: same object returned across modes", test.Code, live.Code)
		}
	})

	t.Run("an over-long key is rejected", func(t *testing.T) {
		rec := e.post(t, "/v1/payment_intents", e.testKey, form, key(strings.Repeat("k", 256)))
		if rec.Code != http.StatusBadRequest || errorCode(t, rec.Body.Bytes()) != "idempotency_key_too_long" {
			t.Fatalf("got %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("50 concurrent requests with one key create one PaymentIntent", func(t *testing.T) {
		const n = 50
		codes := make([]int, n)
		ids := make([]string, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rec := e.post(t, "/v1/payment_intents", e.testKey, url.Values{"amount": {"4242"}, "currency": {"eur"}}, key("burst-1"))
				codes[i] = rec.Code
				var v struct{ ID string }
				_ = json.Unmarshal(rec.Body.Bytes(), &v) // t.Fatal is not allowed off the test goroutine
				ids[i] = v.ID
			}()
		}
		wg.Wait()

		var ok int
		var firstID string
		for i := range n {
			switch codes[i] {
			case 200:
				ok++
				if firstID == "" {
					firstID = ids[i]
				}
				if ids[i] != firstID {
					t.Fatalf("two different PaymentIntents returned: %s and %s", firstID, ids[i])
				}
			case http.StatusConflict:
			default:
				t.Fatalf("request %d: status %d", i, codes[i])
			}
		}
		if ok == 0 {
			t.Fatal("no request succeeded")
		}
		if got := e.count(t, `SELECT count(*) FROM payment_intents WHERE amount = 4242`); got != 1 {
			t.Fatalf("%d PaymentIntents created, want 1", got)
		}
	})
}

func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var v struct {
		Error apiError `json:"error"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	return v.Error.Code
}
