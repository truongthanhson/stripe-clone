package paymentintent_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/truongthanhson/stripe-clone/internal/apikey"
	"github.com/truongthanhson/stripe-clone/internal/cardnetwork"
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

// A hook writes in the change's transaction, so a failing hook must undo the change: this is what
// makes an event exist exactly when its change does.
func TestHookFailureRollsBackTheChange(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t)
	merchant, err := apikey.NewStore(pool).CreateMerchant(ctx, "Acme", "ops@acme.test")
	if err != nil {
		t.Fatal(err)
	}
	store := paymentintent.NewStore(pool)
	var seen []paymentintent.Status
	fail := false
	store.OnChange(func(_ context.Context, _ pgx.Tx, pi paymentintent.PaymentIntent, from paymentintent.Status) error {
		if fail {
			return errors.New("hook failed")
		}
		seen = append(seen, from, pi.Status)
		return nil
	})
	svc := paymentintent.NewService(store, cardnetwork.NewFake(), time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))

	pi, err := svc.Create(ctx, merchant, false, paymentintent.CreateParams{Amount: 2000, Currency: "usd", CaptureMethod: paymentintent.CaptureManual})
	if err != nil {
		t.Fatal(err)
	}
	pm := "pm_card_visa"
	if _, err := svc.Confirm(ctx, merchant, false, pi.ID, &pm); err != nil {
		t.Fatal(err)
	}
	want := []paymentintent.Status{"", "requires_payment_method", "requires_payment_method", "processing", "processing", "requires_capture"}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("hook saw %v, want %v", seen, want)
	}

	fail = true
	if _, err := svc.Cancel(ctx, merchant, false, pi.ID, nil); err == nil {
		t.Fatal("cancel succeeded although its hook failed")
	}
	if got, _ := svc.Get(ctx, merchant, false, pi.ID); got.Status != paymentintent.StatusRequiresCapture {
		t.Fatalf("status = %s after a failed hook, want requires_capture", got.Status)
	}
	if _, err := svc.Create(ctx, merchant, false, paymentintent.CreateParams{Amount: 2000, Currency: "usd"}); err == nil {
		t.Fatal("create succeeded although its hook failed")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM payment_intents`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d payment intents (%v), want 1", n, err)
	}
}
