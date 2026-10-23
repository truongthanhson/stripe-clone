package cardnetwork

import (
	"context"
	"crypto/rand"
	"fmt"
	"sync"
)

// declineCodes maps test card numbers to the decline the fake answers with.
var declineCodes = map[string]string{
	CardDeclined:          "generic_decline",
	CardInsufficientFunds: "insufficient_funds",
	CardLostCard:          "lost_card",
	CardExpired:           "expired_card",
	CardIncorrectCVC:      "incorrect_cvc",
	CardProcessingError:   "processing_error",
}

type holdState int

const (
	holdAuthorized holdState = iota
	holdCaptured
	holdVoided
)

type hold struct {
	auth     Authorization
	amount   int64
	captured int64
	state    holdState
}

// Fake is an in-memory card network for tests and local development. It is safe for concurrent use.
//
// Behaviour by card number: CardVisa and CardMastercard approve; the decline cards answer with
// their decline code; CardTimeoutAfterApprove approves on the network side and then blocks until
// the caller's context ends, returning ErrTimeout, the worst case for a payment system: money held,
// answer lost. Retrying Authorize with the same Reference returns the original authorization.
type Fake struct {
	mu             sync.Mutex
	byReference    map[string]*hold
	byID           map[string]*hold
	authorizeCalls int
}

var _ Network = (*Fake)(nil)

func NewFake() *Fake {
	return &Fake{byReference: map[string]*hold{}, byID: map[string]*hold{}}
}

func (f *Fake) Authorize(ctx context.Context, req AuthorizeRequest) (Authorization, error) {
	f.mu.Lock()
	f.authorizeCalls++
	h, seen := f.byReference[req.Reference]
	if !seen {
		h = &hold{auth: Authorization{ID: "auth_" + rand.Text(), Approved: true}, amount: req.Amount}
		if code, declined := declineCodes[req.Card.Number]; declined {
			h.auth.Approved, h.auth.DeclineCode = false, code
		}
		f.byReference[req.Reference] = h
		f.byID[h.auth.ID] = h
	}
	auth := h.auth
	f.mu.Unlock()

	if !seen && req.Card.Number == CardTimeoutAfterApprove {
		<-ctx.Done()
		return Authorization{}, ErrTimeout
	}
	return auth, nil
}

func (f *Fake) Capture(_ context.Context, authorizationID string, amount int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, ok := f.byID[authorizationID]
	switch {
	case !ok:
		return ErrUnknownAuthorization
	case !h.auth.Approved || h.state == holdVoided || amount <= 0 || amount > h.amount:
		return fmt.Errorf("%w: capture %d", ErrInvalidState, amount)
	case h.state == holdCaptured && h.captured != amount:
		return fmt.Errorf("%w: already captured %d", ErrInvalidState, h.captured)
	}
	h.state, h.captured = holdCaptured, amount
	return nil
}

func (f *Fake) Void(_ context.Context, authorizationID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, ok := f.byID[authorizationID]
	switch {
	case !ok:
		return ErrUnknownAuthorization
	case !h.auth.Approved || h.state == holdCaptured:
		return fmt.Errorf("%w: void", ErrInvalidState)
	}
	h.state = holdVoided
	return nil
}

// Stats is what tests assert on: how much money actually moved on the network.
type Stats struct {
	AuthorizeCalls int   // every call, including retries
	Holds          int   // distinct approved authorizations
	Captured       int64 // total captured amount across all holds
	Captures       int   // holds that were captured
}

func (f *Fake) Stats() Stats {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := Stats{AuthorizeCalls: f.authorizeCalls}
	for _, h := range f.byID {
		if h.auth.Approved {
			s.Holds++
		}
		if h.state == holdCaptured {
			s.Captures++
			s.Captured += h.captured
		}
	}
	return s
}
