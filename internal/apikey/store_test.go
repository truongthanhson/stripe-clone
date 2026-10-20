package apikey_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/truongthanhson/stripe-clone/internal/apikey"
	"github.com/truongthanhson/stripe-clone/migrations"
)

func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	ctr, err := postgres.Run(ctx, "postgres:17", postgres.BasicWaitStrategies())
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	provider, err := goose.NewProvider(goose.DialectPostgres, stdlib.OpenDBFromPool(pool), migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

func TestStore(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := apikey.NewStore(pool)

	merchantID, err := store.CreateMerchant(ctx, "Acme", "ops@acme.test")
	if err != nil {
		t.Fatal(err)
	}
	testKey, err := store.Issue(ctx, merchantID, apikey.ModeTest)
	if err != nil {
		t.Fatal(err)
	}
	liveKey, err := store.Issue(ctx, merchantID, apikey.ModeLive)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("each key resolves to its own mode", func(t *testing.T) {
		p, err := store.Lookup(ctx, testKey.Secret)
		if err != nil {
			t.Fatal(err)
		}
		if p.MerchantID != merchantID || p.Mode != apikey.ModeTest || p.Livemode() {
			t.Fatalf("test key resolved to %+v", p)
		}

		p, err = store.Lookup(ctx, liveKey.Secret)
		if err != nil {
			t.Fatal(err)
		}
		if p.MerchantID != merchantID || p.Mode != apikey.ModeLive || !p.Livemode() {
			t.Fatalf("live key resolved to %+v", p)
		}
	})

	t.Run("a test key with its mode prefix swapped is not a live key", func(t *testing.T) {
		forged := "sk_live_" + testKey.Secret[len("sk_test_"):]
		if _, err := store.Lookup(ctx, forged); !errors.Is(err, apikey.ErrNotFound) {
			t.Fatalf("forged live key lookup err = %v, want ErrNotFound", err)
		}
	})

	t.Run("unknown key is not found", func(t *testing.T) {
		if _, err := store.Lookup(ctx, "sk_test_doesnotexist"); !errors.Is(err, apikey.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("revoked key is not found", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `UPDATE api_keys SET revoked_at = now() WHERE secret_hash = $1`, testKey.Hash); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Lookup(ctx, testKey.Secret); !errors.Is(err, apikey.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("secret is never stored", func(t *testing.T) {
		var n int
		err := pool.QueryRow(ctx,
			`SELECT count(*) FROM api_keys WHERE prefix = $1 OR encode(secret_hash, 'escape') = $1`,
			liveKey.Secret,
		).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatal("found the plaintext secret in api_keys")
		}
	})
}
