package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/truongthanhson/stripe-clone/internal/paymentintent"
	"github.com/truongthanhson/stripe-clone/internal/refund"
)

// resolver settles payments and refunds whose card network call got no answer. Until it does, they
// sit in an explicit unknown state (processing, or pending with unresolved_since set), with the money
// neither counted nor released.
type resolver struct {
	intents *paymentintent.Service
	refunds *refund.Service
	// grace is how long an unmarked processing payment or pending refund may be in flight before it
	// counts as abandoned. It is above the network timeout, so a live request is never raced.
	grace time.Duration
}

// once makes one pass over everything unknown and returns how many reached a final answer.
func (r resolver) once(ctx context.Context) (int, error) {
	n, err := r.intents.ResolveUnknown(ctx, r.grace)
	if err != nil {
		return n, err
	}
	m, err := r.refunds.ResolveUnknown(ctx, r.grace)
	return n + m, err
}

func (r resolver) run(ctx context.Context, logger *slog.Logger, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		n, err := r.once(ctx)
		if err != nil && ctx.Err() == nil {
			logger.Error("resolve unknown outcomes", "error", err)
		}
		if n > 0 {
			logger.Info("resolved unknown outcomes", "settled", n)
		}
	}
}
