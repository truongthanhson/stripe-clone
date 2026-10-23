package paymentintent_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/truongthanhson/stripe-clone/internal/apikey"
	"github.com/truongthanhson/stripe-clone/internal/paymentintent"
	"github.com/truongthanhson/stripe-clone/internal/testdb"
)

func TestStore(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t)
	store := paymentintent.NewStore(pool)
	merchants := apikey.NewStore(pool)

	acme, err := merchants.CreateMerchant(ctx, "Acme", "ops@acme.test")
	if err != nil {
		t.Fatal(err)
	}
	globex, err := merchants.CreateMerchant(ctx, "Globex", "ops@globex.test")
	if err != nil {
		t.Fatal(err)
	}

	desc := "Order #1"
	created, err := store.Create(ctx, acme, false, paymentintent.CreateParams{Amount: 2000, Currency: "usd", Description: &desc})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("create sets defaults", func(t *testing.T) {
		if created.Status != paymentintent.StatusRequiresPaymentMethod || created.CaptureMethod != paymentintent.CaptureAutomatic ||
			created.Livemode || created.MerchantID != acme || created.Created.IsZero() {
			t.Fatalf("created = %+v", created)
		}
	})

	t.Run("get returns what create stored", func(t *testing.T) {
		got, err := store.Get(ctx, acme, false, created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, created) {
			t.Fatalf("got %+v, want %+v", got, created)
		}
	})

	t.Run("live mode cannot see test data", func(t *testing.T) {
		if _, err := store.Get(ctx, acme, true, created.ID); !errors.Is(err, paymentintent.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("another merchant cannot see it", func(t *testing.T) {
		if _, err := store.Get(ctx, globex, false, created.ID); !errors.Is(err, paymentintent.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("invalid params are rejected before the database", func(t *testing.T) {
		_, err := store.Create(ctx, acme, false, paymentintent.CreateParams{Amount: 1, Currency: "usd"})
		var ve *paymentintent.ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("err = %v, want ValidationError", err)
		}
	})

	t.Run("the database also refuses a non-positive amount", func(t *testing.T) {
		_, err := pool.Exec(ctx, `INSERT INTO payment_intents (id, merchant_id, livemode, amount, currency, status, capture_method)
			VALUES ('pi_bad', $1, false, 0, 'usd', 'requires_payment_method', 'automatic')`, acme)
		if err == nil {
			t.Fatal("insert with amount 0 succeeded")
		}
	})
}
