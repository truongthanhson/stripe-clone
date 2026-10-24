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
	card     string
	amount   int64
	captured int64
	refunded int64
	state    holdState
}

// Fake is an in-memory card network for tests and local development. It is safe for concurrent use.
//
// Behaviour by card number: CardVisa and CardMastercard approve; the decline cards answer with
// their decline code; CardTimeoutAfterApprove approves on the network side and then blocks until
// the caller's context ends, returning ErrTimeout, the worst case for a payment system: money held,
// answer lost. Retrying Authorize with the same Reference returns the original authorization.
// CardRefundFails captures normally but refuses every refund. Holds live in memory only, so a
// restart forgets them.
type Fake struct {
	mu             sync.Mutex
	byReference    map[string]*hold
	byID           map[string]*hold
	refunds        map[string]int64 // amount by refund reference
	authorizeCalls int
}

var _ Network = (*Fake)(nil)

func NewFake() *Fake {
	return &Fake{byReference: map[string]*hold{}, byID: map[string]*hold{}, refunds: map[string]int64{}}
}

func (f *Fake) Authorize(ctx context.Context, req AuthorizeRequest) (Authorization, error) {
	f.mu.Lock()
	f.authorizeCalls++
	h, seen := f.byReference[req.Reference]
	if !seen {
		h = &hold{auth: Authorization{ID: "auth_" + rand.Text(), Approved: true}, card: req.Card.Number, amount: req.Amount}
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

func (f *Fake) Refund(_ context.Context, req RefundRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if amount, seen := f.refunds[req.Reference]; seen {
		if amount != req.Amount {
			return fmt.Errorf("%w: refund %s already made for %d", ErrInvalidState, req.Reference, amount)
		}
		return nil
	}
	h, ok := f.byID[req.AuthorizationID]
	switch {
	case !ok:
		return ErrUnknownAuthorization
	case h.state != holdCaptured || req.Amount <= 0 || h.refunded+req.Amount > h.captured:
		return fmt.Errorf("%w: refund %d of %d captured, %d already refunded", ErrInvalidState, req.Amount, h.captured, h.refunded)
	case h.card == CardRefundFails:
		return ErrRefundFailed
	}
	h.refunded += req.Amount
	f.refunds[req.Reference] = req.Amount
	return nil
}

// Stats is what tests assert on: how much money actually moved on the network.
type Stats struct {
	AuthorizeCalls int   // every call, including retries
	Holds          int   // distinct approved authorizations
	Captured       int64 // total captured amount across all holds
	Captures       int   // holds that were captured
	Refunded       int64 // total refunded across all holds
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
			s.Refunded += h.refunded
		}
	}
	return s
}

// Hold is one authorization as the fake bank sees it, for tests that compare the bank with our books.
type Hold struct {
	Reference       string
	AuthorizationID string
	Approved        bool
	Amount          int64
	Captured        int64 // 0 unless captured
	Refunded        int64
	Voided          bool
}

// Holds lists every authorization the fake has answered, approved or not.
func (f *Fake) Holds() []Hold {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Hold, 0, len(f.byReference))
	for ref, h := range f.byReference {
		hold := Hold{Reference: ref, AuthorizationID: h.auth.ID, Approved: h.auth.Approved, Amount: h.amount,
			Refunded: h.refunded, Voided: h.state == holdVoided}
		if h.state == holdCaptured {
			hold.Captured = h.captured
		}
		out = append(out, hold)
	}
	return out
}
