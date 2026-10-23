package cardnetwork

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func card(t *testing.T, pm string) Card {
	t.Helper()
	c, ok := TestPaymentMethod(pm)
	if !ok {
		t.Fatalf("unknown test payment method %q", pm)
	}
	return c
}

func TestFakeAuthorizeOutcomes(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		pm          string
		approved    bool
		declineCode string
	}{
		{"pm_card_visa", true, ""},
		{"pm_card_mastercard", true, ""},
		{"pm_card_chargeDeclined", false, "generic_decline"},
		{"pm_card_chargeDeclinedInsufficientFunds", false, "insufficient_funds"},
		{"pm_card_chargeDeclinedLostCard", false, "lost_card"},
		{"pm_card_chargeDeclinedExpiredCard", false, "expired_card"},
		{"pm_card_chargeDeclinedIncorrectCvc", false, "incorrect_cvc"},
		{"pm_card_chargeDeclinedProcessingError", false, "processing_error"},
	}
	f := NewFake()
	for _, tt := range tests {
		t.Run(tt.pm, func(t *testing.T) {
			auth, err := f.Authorize(ctx, AuthorizeRequest{Reference: tt.pm, Card: card(t, tt.pm), Amount: 1000, Currency: "usd"})
			if err != nil {
				t.Fatal(err)
			}
			if auth.Approved != tt.approved || auth.DeclineCode != tt.declineCode || auth.ID == "" {
				t.Fatalf("auth = %+v, want approved=%v decline=%q", auth, tt.approved, tt.declineCode)
			}
		})
	}
}

func TestFakeTimeoutThenRetry(t *testing.T) {
	f := NewFake()
	req := AuthorizeRequest{Reference: "pi_1", Card: card(t, "pm_card_timeout"), Amount: 1000, Currency: "usd"}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := f.Authorize(ctx, req); !errors.Is(err, ErrTimeout) {
		t.Fatalf("first call err = %v, want ErrTimeout", err)
	}
	if s := f.Stats(); s.Holds != 1 {
		t.Fatalf("holds after timeout = %d, want 1: the network approved even though we never heard", s.Holds)
	}

	auth, err := f.Authorize(context.Background(), req)
	if err != nil || !auth.Approved {
		t.Fatalf("retry = %+v, %v; want the original approval", auth, err)
	}
	if s := f.Stats(); s.Holds != 1 || s.AuthorizeCalls != 2 {
		t.Fatalf("stats = %+v, want 1 hold from 2 calls", s)
	}
}

func TestFakeAuthorizeIsIdempotentUnderConcurrency(t *testing.T) {
	f := NewFake()
	req := AuthorizeRequest{Reference: "pi_same", Card: card(t, "pm_card_visa"), Amount: 1000, Currency: "usd"}
	ids := make([]string, 20)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			auth, _ := f.Authorize(context.Background(), req)
			ids[i] = auth.ID
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("got authorizations %s and %s for one reference", ids[0], id)
		}
	}
	if s := f.Stats(); s.Holds != 1 {
		t.Fatalf("holds = %d, want 1", s.Holds)
	}
}

func TestFakeCaptureAndVoid(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	authorize := func(ref, pm string) string {
		t.Helper()
		auth, err := f.Authorize(ctx, AuthorizeRequest{Reference: ref, Card: card(t, pm), Amount: 1000, Currency: "usd"})
		if err != nil {
			t.Fatal(err)
		}
		return auth.ID
	}

	t.Run("capture, then the same capture again, is one capture", func(t *testing.T) {
		id := authorize("c1", "pm_card_visa")
		if err := f.Capture(ctx, id, 1000); err != nil {
			t.Fatal(err)
		}
		if err := f.Capture(ctx, id, 1000); err != nil {
			t.Fatalf("repeated capture: %v", err)
		}
		if err := f.Capture(ctx, id, 500); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("different second capture err = %v", err)
		}
		if err := f.Void(ctx, id); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("void after capture err = %v", err)
		}
	})

	t.Run("partial capture, never more than authorized", func(t *testing.T) {
		id := authorize("c2", "pm_card_visa")
		if err := f.Capture(ctx, id, 1001); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("over-capture err = %v", err)
		}
		if err := f.Capture(ctx, id, 600); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("voided hold cannot be captured", func(t *testing.T) {
		id := authorize("c3", "pm_card_visa")
		if err := f.Void(ctx, id); err != nil {
			t.Fatal(err)
		}
		if err := f.Capture(ctx, id, 1000); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("capture after void err = %v", err)
		}
	})

	t.Run("declined authorization cannot be captured", func(t *testing.T) {
		id := authorize("c4", "pm_card_chargeDeclined")
		if err := f.Capture(ctx, id, 1000); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("unknown authorization", func(t *testing.T) {
		if err := f.Capture(ctx, "auth_nope", 1); !errors.Is(err, ErrUnknownAuthorization) {
			t.Fatalf("err = %v", err)
		}
	})

	if s := f.Stats(); s.Captured != 1600 || s.Captures != 2 {
		t.Fatalf("stats = %+v, want 1600 captured over 2 holds", s)
	}
}
