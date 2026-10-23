package paymentintent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/truongthanhson/stripe-clone/internal/cardnetwork"
)

// Service runs the PaymentIntent lifecycle: it moves the state machine and talks to the card network.
//
// The rule that keeps money safe: never hold a database transaction open across a network call.
// Instead, claim the PaymentIntent with a compare-and-swap (requires_confirmation -> processing),
// call the network with no lock held, then record the answer with a second compare-and-swap.
type Service struct {
	*Store
	network cardnetwork.Network
	timeout time.Duration
	logger  *slog.Logger
}

// NewService wires the store to a card network. timeout bounds each network call; it must be
// shorter than the HTTP server's write timeout.
func NewService(store *Store, network cardnetwork.Network, timeout time.Duration, logger *slog.Logger) *Service {
	return &Service{Store: store, network: network, timeout: timeout, logger: logger}
}

// networkContext detaches from the caller's cancellation: once a request reaches the network, a
// client hanging up must not abandon it halfway; only the timeout ends it.
func (s *Service) networkContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), s.timeout)
}

// Confirm charges (or, with manual capture, authorizes) the PaymentIntent's payment method.
// paymentMethod, when set, replaces the one on the PaymentIntent.
func (s *Service) Confirm(ctx context.Context, merchantID int64, livemode bool, id string, paymentMethod *string) (PaymentIntent, error) {
	pi, err := s.Get(ctx, merchantID, livemode, id)
	if err != nil {
		return PaymentIntent{}, err
	}
	if pi.Status != StatusRequiresPaymentMethod && pi.Status != StatusRequiresConfirmation {
		return PaymentIntent{}, &StateError{Action: "confirm", Status: pi.Status}
	}
	if paymentMethod == nil {
		paymentMethod = pi.PaymentMethod
	}
	card, err := resolveCard(livemode, paymentMethod)
	if err != nil {
		return PaymentIntent{}, err
	}

	// Claim: only one concurrent confirm gets past this line.
	pi, err = s.transition(ctx, pi.ID, pi.Status, StatusProcessing,
		`payment_method = $4, last_payment_error = NULL, attempts = attempts + 1`, *paymentMethod)
	if errors.Is(err, ErrStaleStatus) {
		return PaymentIntent{}, s.stateErrorNow(ctx, merchantID, livemode, id, "confirm")
	}
	if err != nil {
		return PaymentIntent{}, err
	}

	nctx, cancel := s.networkContext(ctx)
	defer cancel()
	auth, err := s.network.Authorize(nctx, cardnetwork.AuthorizeRequest{
		Reference: pi.networkReference(), Card: card, Amount: pi.Amount, Currency: pi.Currency,
	})
	if err != nil {
		// Outcome unknown: the card may be charged. Stay in processing; never guess "declined".
		// Phase 3 adds the job that asks the network again with the same reference and settles it.
		s.logger.Warn("authorize outcome unknown; payment intent left processing", "payment_intent", pi.ID, "error", err)
		return pi, nil
	}

	if !auth.Approved {
		pi, err = s.transition(ctx, pi.ID, StatusProcessing, StatusRequiresPaymentMethod,
			`payment_method = NULL, last_payment_error = $4`, declineError(auth.DeclineCode, *paymentMethod))
		if err != nil {
			return PaymentIntent{}, err
		}
		return PaymentIntent{}, &DeclineError{Intent: pi}
	}

	if pi.CaptureMethod == CaptureManual {
		return s.transition(ctx, pi.ID, StatusProcessing, StatusRequiresCapture,
			`authorization_id = $4, amount_capturable = amount`, auth.ID)
	}

	if err := s.network.Capture(nctx, auth.ID, pi.Amount); err != nil {
		// Authorized but capture unknown. Record the authorization so the Phase 3 resolver can finish it.
		s.logger.Warn("capture outcome unknown; payment intent left processing", "payment_intent", pi.ID, "error", err)
		return s.transition(ctx, pi.ID, StatusProcessing, StatusProcessing, `authorization_id = $4`, auth.ID)
	}
	amount := pi.Amount
	pi, err = s.succeed(ctx, pi.ID, StatusProcessing, `authorization_id = $4, amount_received = amount`, auth.ID)
	if err != nil {
		s.logger.Error("captured on the network but not recorded", "payment_intent", id, "amount", amount, "error", err)
	}
	return pi, err
}

// Capture takes the authorized money of a requires_capture PaymentIntent. amount defaults to all of
// it; capturing less releases the rest, as Stripe does.
func (s *Service) Capture(ctx context.Context, merchantID int64, livemode bool, id string, amount *int64) (PaymentIntent, error) {
	pi, err := s.Get(ctx, merchantID, livemode, id)
	if err != nil {
		return PaymentIntent{}, err
	}
	if pi.Status != StatusRequiresCapture {
		return PaymentIntent{}, &StateError{Action: "capture", Status: pi.Status}
	}
	toCapture := pi.AmountCapturable
	if amount != nil {
		toCapture = *amount
	}
	// Like a new charge, a capture must be at least the currency's minimum: below it the fee would
	// swallow the whole amount.
	if minAmount := minAmounts[pi.Currency]; toCapture < minAmount {
		return PaymentIntent{}, &ValidationError{"amount_to_capture", "amount_too_small",
			fmt.Sprintf("Amount to capture must be at least %d %s.", minAmount, pi.Currency)}
	}
	if toCapture > pi.AmountCapturable {
		return PaymentIntent{}, &ValidationError{"amount_to_capture", "amount_too_large",
			fmt.Sprintf("Amount to capture must be at most the capturable amount, %d.", pi.AmountCapturable)}
	}

	// Two concurrent captures both reach the network, which is idempotent on the authorization;
	// only one of the status updates below lands.
	nctx, cancel := s.networkContext(ctx)
	defer cancel()
	if err := s.network.Capture(nctx, *pi.AuthorizationID, toCapture); err != nil {
		if errors.Is(err, cardnetwork.ErrInvalidState) {
			return PaymentIntent{}, s.stateErrorNow(ctx, merchantID, livemode, id, "capture")
		}
		s.logger.Warn("capture failed", "payment_intent", pi.ID, "error", err)
		return PaymentIntent{}, ErrNetworkUnavailable
	}
	pi, err = s.succeed(ctx, pi.ID, StatusRequiresCapture, `amount_received = $4, amount_capturable = 0`, toCapture)
	if errors.Is(err, ErrStaleStatus) {
		return PaymentIntent{}, s.stateErrorNow(ctx, merchantID, livemode, id, "capture")
	}
	if err != nil {
		// The network moved the money but we could not record it. A retry of the same capture is
		// safe (the network capture is idempotent); Phase 3's resolver will also catch these.
		s.logger.Error("captured on the network but not recorded", "payment_intent", id, "amount", toCapture, "error", err)
	}
	return pi, err
}

var cancellationReasons = map[string]bool{"duplicate": true, "fraudulent": true, "requested_by_customer": true, "abandoned": true}

// Cancel ends a PaymentIntent that has not succeeded. A held authorization is voided first, so the
// customer's money is released before we report canceled.
func (s *Service) Cancel(ctx context.Context, merchantID int64, livemode bool, id string, reason *string) (PaymentIntent, error) {
	if reason != nil && !cancellationReasons[*reason] {
		return PaymentIntent{}, &ValidationError{"cancellation_reason", "parameter_invalid_string",
			"Invalid cancellation_reason: must be one of duplicate, fraudulent, requested_by_customer or abandoned."}
	}
	pi, err := s.Get(ctx, merchantID, livemode, id)
	if err != nil {
		return PaymentIntent{}, err
	}
	if CheckTransition(pi.Status, StatusCanceled) != nil {
		return PaymentIntent{}, &StateError{Action: "cancel", Status: pi.Status}
	}

	if pi.Status == StatusRequiresCapture {
		nctx, cancel := s.networkContext(ctx)
		defer cancel()
		if err := s.network.Void(nctx, *pi.AuthorizationID); err != nil {
			if errors.Is(err, cardnetwork.ErrInvalidState) {
				return PaymentIntent{}, s.stateErrorNow(ctx, merchantID, livemode, id, "cancel")
			}
			s.logger.Warn("void failed", "payment_intent", pi.ID, "error", err)
			return PaymentIntent{}, ErrNetworkUnavailable
		}
	}
	pi, err = s.transition(ctx, pi.ID, pi.Status, StatusCanceled,
		`canceled_at = now(), cancellation_reason = $4, amount_capturable = 0`, reason)
	if errors.Is(err, ErrStaleStatus) {
		return PaymentIntent{}, s.stateErrorNow(ctx, merchantID, livemode, id, "cancel")
	}
	return pi, err
}

// stateErrorNow reports the status a concurrent request left the PaymentIntent in.
func (s *Service) stateErrorNow(ctx context.Context, merchantID int64, livemode bool, id, action string) error {
	pi, err := s.Get(ctx, merchantID, livemode, id)
	if err != nil {
		return err
	}
	return &StateError{Action: action, Status: pi.Status}
}

func resolveCard(livemode bool, paymentMethod *string) (cardnetwork.Card, error) {
	if paymentMethod == nil {
		return cardnetwork.Card{}, &ValidationError{"payment_method", "parameter_missing",
			"You cannot confirm this PaymentIntent because it's missing a payment method. Pass payment_method."}
	}
	if livemode {
		return cardnetwork.Card{}, &ValidationError{"payment_method", "livemode_unavailable",
			"Live payments are not available yet; use a test key."}
	}
	card, ok := cardnetwork.TestPaymentMethod(*paymentMethod)
	if !ok {
		return cardnetwork.Card{}, &ValidationError{"payment_method", "resource_missing",
			fmt.Sprintf("No such PaymentMethod: '%s'", *paymentMethod)}
	}
	return card, nil
}

var declineMessages = map[string]string{
	"generic_decline":    "Your card was declined.",
	"insufficient_funds": "Your card has insufficient funds.",
	"lost_card":          "Your card was declined.",
	"expired_card":       "Your card has expired.",
	"incorrect_cvc":      "Your card's security code is incorrect.",
	"processing_error":   "An error occurred while processing your card. Try again in a little bit.",
}

func declineError(code, paymentMethod string) PaymentError {
	msg, ok := declineMessages[code]
	if !ok {
		msg = "Your card was declined."
	}
	return PaymentError{Type: "card_error", Code: "card_declined", DeclineCode: code, Message: msg, PaymentMethod: paymentMethod}
}
