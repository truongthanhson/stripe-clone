package idempotency_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/truongthanhson/stripe-clone/internal/apikey"
	"github.com/truongthanhson/stripe-clone/internal/idempotency"
	"github.com/truongthanhson/stripe-clone/internal/testdb"
)

func TestFingerprint(t *testing.T) {
	a := idempotency.Fingerprint("POST", "/v1/payment_intents", []byte("amount=2000&currency=usd"))
	b := idempotency.Fingerprint("POST", "/v1/payment_intents", []byte("currency=usd&amount=2000"))
	if !bytes.Equal(a, b) {
		t.Error("reordered parameters changed the fingerprint")
	}
	if bytes.Equal(a, idempotency.Fingerprint("POST", "/v1/payment_intents", []byte("amount=2001&currency=usd"))) {
		t.Error("a different amount has the same fingerprint")
	}
	if bytes.Equal(a, idempotency.Fingerprint("POST", "/v1/refunds", []byte("amount=2000&currency=usd"))) {
		t.Error("a different path has the same fingerprint")
	}
}

func TestStore(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t)
	store := idempotency.NewStore(pool)

	merchants := apikey.NewStore(pool)
	acme, _ := merchants.CreateMerchant(ctx, "Acme", "ops@acme.test")
	globex, _ := merchants.CreateMerchant(ctx, "Globex", "ops@globex.test")

	fp := idempotency.Fingerprint("POST", "/v1/payment_intents", []byte("amount=2000&currency=usd"))
	other := idempotency.Fingerprint("POST", "/v1/payment_intents", []byte("amount=5000&currency=usd"))

	begin := func(sc idempotency.Scope, fp []byte) (*idempotency.Response, error) {
		t.Helper()
		return store.Begin(ctx, sc, fp)
	}
	age := func(sc idempotency.Scope, column, interval string) {
		t.Helper()
		_, err := pool.Exec(ctx, `UPDATE idempotency_keys SET `+column+` = now() - $4::interval
			WHERE merchant_id = $1 AND livemode = $2 AND key = $3`, sc.MerchantID, sc.Livemode, sc.Key, interval)
		if err != nil {
			t.Fatal(err)
		}
	}

	t.Run("first request runs, a concurrent one is refused, a later one replays", func(t *testing.T) {
		sc := idempotency.Scope{MerchantID: acme, Key: "k1"}
		if resp, err := begin(sc, fp); resp != nil || err != nil {
			t.Fatalf("first begin = %v, %v; want run", resp, err)
		}
		if _, err := begin(sc, fp); !errors.Is(err, idempotency.ErrInProgress) {
			t.Fatalf("second begin err = %v, want ErrInProgress", err)
		}
		if err := store.Complete(ctx, sc, idempotency.Response{Status: 200, Body: []byte(`{"id":"pi_1"}`)}); err != nil {
			t.Fatal(err)
		}
		resp, err := begin(sc, fp)
		if err != nil || resp == nil || resp.Status != 200 || string(resp.Body) != `{"id":"pi_1"}` {
			t.Fatalf("replay = %+v, %v", resp, err)
		}
	})

	t.Run("different parameters with the same key are refused", func(t *testing.T) {
		sc := idempotency.Scope{MerchantID: acme, Key: "k1"}
		if _, err := begin(sc, other); !errors.Is(err, idempotency.ErrMismatch) {
			t.Fatalf("err = %v, want ErrMismatch", err)
		}
	})

	t.Run("keys are scoped by merchant and mode", func(t *testing.T) {
		for _, sc := range []idempotency.Scope{{MerchantID: globex, Key: "k1"}, {MerchantID: acme, Livemode: true, Key: "k1"}} {
			if resp, err := begin(sc, other); resp != nil || err != nil {
				t.Fatalf("begin %+v = %v, %v; want run", sc, resp, err)
			}
		}
	})

	t.Run("released key runs again", func(t *testing.T) {
		sc := idempotency.Scope{MerchantID: acme, Key: "k2"}
		if _, err := begin(sc, fp); err != nil {
			t.Fatal(err)
		}
		if err := store.Release(ctx, sc); err != nil {
			t.Fatal(err)
		}
		if resp, err := begin(sc, other); resp != nil || err != nil {
			t.Fatalf("after release = %v, %v; want run", resp, err)
		}
	})

	t.Run("key expires after 24 hours", func(t *testing.T) {
		sc := idempotency.Scope{MerchantID: acme, Key: "k3"}
		_, _ = begin(sc, fp)
		_ = store.Complete(ctx, sc, idempotency.Response{Status: 200, Body: []byte("{}")})

		age(sc, "created_at", "23 hours")
		if _, err := begin(sc, other); !errors.Is(err, idempotency.ErrMismatch) {
			t.Fatalf("at 23h err = %v, want ErrMismatch", err)
		}
		age(sc, "created_at", "25 hours")
		if resp, err := begin(sc, other); resp != nil || err != nil {
			t.Fatalf("at 25h = %v, %v; want run", resp, err)
		}
	})

	t.Run("an abandoned in-progress key is taken over only by the same request", func(t *testing.T) {
		sc := idempotency.Scope{MerchantID: acme, Key: "k4"}
		_, _ = begin(sc, fp)
		age(sc, "locked_at", "2 minutes")
		if _, err := begin(sc, other); !errors.Is(err, idempotency.ErrMismatch) {
			t.Fatalf("different request err = %v, want ErrMismatch", err)
		}
		if resp, err := begin(sc, fp); resp != nil || err != nil {
			t.Fatalf("same request = %v, %v; want run", resp, err)
		}
	})

	// Many requests race for a key that may be taken over. Postgres serialises them on the row
	// lock and re-checks the WHERE against the row the winner wrote, so the takeover must leave
	// a row that no longer qualifies: created_at and locked_at are reset to now().
	race := func(t *testing.T, sc idempotency.Scope, fps func(i int) []byte) {
		t.Helper()
		const n = 16
		start := make(chan struct{})
		var wg sync.WaitGroup
		var mu sync.Mutex
		winners := 0
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				resp, err := store.Begin(ctx, sc, fps(i))
				if resp == nil && err == nil {
					mu.Lock()
					winners++
					mu.Unlock()
				} else if err != nil && !errors.Is(err, idempotency.ErrInProgress) && !errors.Is(err, idempotency.ErrMismatch) {
					t.Errorf("begin: %v", err)
				}
			}()
		}
		close(start)
		wg.Wait()
		if winners != 1 {
			t.Fatalf("%d requests claimed the key, want exactly 1", winners)
		}
	}

	t.Run("concurrent requests on an expired key: exactly one runs", func(t *testing.T) {
		for round := range 20 {
			sc := idempotency.Scope{MerchantID: acme, Key: fmt.Sprintf("expired-%d", round)}
			_, _ = begin(sc, fp)
			_ = store.Complete(ctx, sc, idempotency.Response{Status: 200, Body: []byte("{}")})
			age(sc, "created_at", "25 hours")
			// Half send the old request, half a new one: an expired key may be reused for anything.
			race(t, sc, func(i int) []byte {
				if i%2 == 0 {
					return fp
				}
				return other
			})
		}
	})

	t.Run("concurrent retries of an abandoned in-progress key: exactly one runs", func(t *testing.T) {
		for round := range 20 {
			sc := idempotency.Scope{MerchantID: acme, Key: fmt.Sprintf("abandoned-%d", round)}
			_, _ = begin(sc, fp)
			age(sc, "locked_at", "2 minutes")
			race(t, sc, func(int) []byte { return fp })
		}
	})

	t.Run("prune deletes expired keys", func(t *testing.T) {
		sc := idempotency.Scope{MerchantID: acme, Key: "k5"}
		_, _ = begin(sc, fp)
		age(sc, "created_at", "2 days")
		n, err := store.Prune(ctx)
		if err != nil || n != 1 {
			t.Fatalf("prune = %d, %v; want 1", n, err)
		}
	})
}
