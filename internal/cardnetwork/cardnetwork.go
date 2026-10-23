// Package cardnetwork is the boundary to whoever moves money on a card: today an in-process fake,
// later a Stripe test-mode adapter. Nothing outside this package knows which one is plugged in.
package cardnetwork

import (
	"context"
	"errors"
)

// Network authorizes, captures and voids card payments.
//
// Authorize must be idempotent on Reference: calling it again with a reference it has seen returns
// the original authorization instead of placing a second hold. That is what makes retrying after a
// timeout safe.
type Network interface {
	Authorize(ctx context.Context, req AuthorizeRequest) (Authorization, error)
	// Capture moves up to the authorized amount. Capturing an already captured authorization
	// with the same amount succeeds again, so a retried capture is harmless.
	Capture(ctx context.Context, authorizationID string, amount int64) error
	// Void releases an uncaptured hold.
	Void(ctx context.Context, authorizationID string) error
}

type AuthorizeRequest struct {
	Reference string // our ID for the attempt; the network deduplicates on it
	Card      Card
	Amount    int64
	Currency  string
}

// Authorization is the network's answer. A decline is an answer, not an error.
type Authorization struct {
	ID          string
	Approved    bool
	DeclineCode string // set when !Approved, e.g. "insufficient_funds"
}

var (
	// ErrTimeout means no answer arrived in time: the card may or may not have been authorized.
	// Callers must not treat it as a decline.
	ErrTimeout = errors.New("card network did not answer in time; outcome unknown")
	// ErrUnknownAuthorization means the network has no authorization with that ID.
	ErrUnknownAuthorization = errors.New("unknown authorization")
	// ErrInvalidState means the operation does not fit the authorization's state,
	// for example capturing a voided hold or capturing more than was authorized.
	ErrInvalidState = errors.New("authorization is not in a state that allows this")
)
