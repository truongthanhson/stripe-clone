// Package refund returns captured money to a customer's card, in full or in parts.
package refund

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongthanhson/stripe-clone/internal/cardnetwork"
	"github.com/truongthanhson/stripe-clone/internal/ledger"
	"github.com/truongthanhson/stripe-clone/internal/listing"
	"github.com/truongthanhson/stripe-clone/internal/metadata"
)

type Status string

const (
	StatusPending   Status = "pending"   // sent to the network, answer not yet known
	StatusSucceeded Status = "succeeded" // money returned and recorded in the ledger
	StatusFailed    Status = "failed"    // the issuer refused; no money moved
)

type Refund struct {
	ID              string
	MerchantID      int64
	Livemode        bool
	PaymentIntentID string
	Amount          int64
	Currency        string
	Status          Status
	Reason          *string
	FailureReason   *string
	Metadata        map[string]string
	Created         time.Time
}

type CreateParams struct {
	PaymentIntentID string
	Amount          *int64 // nil refunds everything still refundable
	Reason          *string
	Metadata        metadata.Update
}

var ErrNotFound = errors.New("refund not found")

// ValidationError names the parameter that is wrong.
type ValidationError struct {
	Param   string
	Code    string
	Message string
}

func (e *ValidationError) Error() string { return e.Param + ": " + e.Message }

var reasons = map[string]bool{"duplicate": true, "fraudulent": true, "requested_by_customer": true}

type Service struct {
	pool    *pgxpool.Pool
	network cardnetwork.Network
	timeout time.Duration
	logger  *slog.Logger
}

func NewService(pool *pgxpool.Pool, network cardnetwork.Network, timeout time.Duration, logger *slog.Logger) *Service {
	return &Service{pool: pool, network: network, timeout: timeout, logger: logger}
}

const columns = `id, merchant_id, livemode, payment_intent_id, amount, currency, status, reason, failure_reason, metadata, created_at`

func scan(row pgx.Row) (Refund, error) {
	var r Refund
	err := row.Scan(&r.ID, &r.MerchantID, &r.Livemode, &r.PaymentIntentID, &r.Amount, &r.Currency, &r.Status,
		&r.Reason, &r.FailureReason, &r.Metadata, &r.Created)
	if errors.Is(err, pgx.ErrNoRows) {
		return Refund{}, ErrNotFound
	}
	return r, err
}

// Create refunds part or all of a succeeded PaymentIntent.
//
// Step 1 reserves the amount: with the PaymentIntent row locked, it checks what is still refundable
// (received minus pending and succeeded refunds) and inserts a pending refund. Concurrent refunds of
// one PaymentIntent queue on that lock, so together they can never exceed what was received.
// Step 2 calls the network with no lock held. Step 3 records the outcome; a success posts to the
// ledger in the same transaction.
func (s *Service) Create(ctx context.Context, merchantID int64, livemode bool, p CreateParams) (Refund, error) {
	if p.Reason != nil && !reasons[*p.Reason] {
		return Refund{}, &ValidationError{"reason", "parameter_invalid_string",
			"Invalid reason: must be one of duplicate, fraudulent or requested_by_customer."}
	}
	md, err := p.Metadata.Apply(nil)
	if err != nil {
		return Refund{}, err
	}

	var re Refund
	var authorizationID string
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Refunds read the payments table directly: they need its row lock, which is the point.
		var status, currency string
		var received int64
		var authID *string
		err := tx.QueryRow(ctx, `
			SELECT status, currency, amount_received, authorization_id FROM payment_intents
			WHERE id = $1 AND merchant_id = $2 AND livemode = $3 FOR UPDATE`,
			p.PaymentIntentID, merchantID, livemode,
		).Scan(&status, &currency, &received, &authID)
		if errors.Is(err, pgx.ErrNoRows) {
			return &ValidationError{"payment_intent", "resource_missing", fmt.Sprintf("No such payment_intent: '%s'", p.PaymentIntentID)}
		}
		if err != nil {
			return err
		}
		if status != "succeeded" {
			return &ValidationError{"payment_intent", "charge_not_refundable", fmt.Sprintf(
				"This PaymentIntent has a status of %s; only succeeded PaymentIntents can be refunded. Cancel it instead.", status)}
		}

		var reserved int64
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(SUM(amount), 0) FROM refunds WHERE payment_intent_id = $1 AND status IN ('pending', 'succeeded')`,
			p.PaymentIntentID).Scan(&reserved); err != nil {
			return err
		}
		refundable := received - reserved
		amount := refundable
		if p.Amount != nil {
			amount = *p.Amount
		}
		switch {
		case refundable == 0:
			return &ValidationError{"payment_intent", "charge_already_refunded", "This PaymentIntent has already been fully refunded."}
		case amount <= 0 || amount > refundable:
			return &ValidationError{"amount", "amount_too_large", fmt.Sprintf(
				"Refund amount must be between 1 and the refundable amount, %d.", refundable)}
		}

		authorizationID = *authID
		re, err = scan(tx.QueryRow(ctx, `
			INSERT INTO refunds (id, merchant_id, livemode, payment_intent_id, amount, currency, status, reason, metadata)
			VALUES ($1, $2, $3, $4, $5, $6, 'pending', $7, $8) RETURNING `+columns,
			"re_"+rand.Text(), merchantID, livemode, p.PaymentIntentID, amount, currency, p.Reason, md))
		return err
	})
	if err != nil {
		return Refund{}, err
	}

	nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.timeout)
	defer cancel()
	err = s.network.Refund(nctx, cardnetwork.RefundRequest{Reference: re.ID, AuthorizationID: authorizationID, Amount: re.Amount})
	switch {
	case errors.Is(err, cardnetwork.ErrRefundFailed):
		return s.fail(ctx, re.ID, "expired_or_canceled_card")
	case errors.Is(err, cardnetwork.ErrUnknownAuthorization), errors.Is(err, cardnetwork.ErrInvalidState):
		// A definite "no" from the network: nothing was paid out, so the refund fails and the
		// amount becomes refundable again.
		s.logger.Error("network refused refund", "refund", re.ID, "error", err)
		return s.fail(ctx, re.ID, "unknown")
	case err != nil:
		// Unknown outcome: keep the amount reserved and the refund pending. Never guess.
		s.logger.Warn("refund outcome unknown; refund left pending", "refund", re.ID, "error", err)
		return re, nil
	}

	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		re, err = scan(tx.QueryRow(ctx, `
			UPDATE refunds SET status = 'succeeded', updated_at = now()
			WHERE id = $1 AND status = 'pending' RETURNING `+columns, re.ID))
		if err != nil {
			return err
		}
		_, err = ledger.Post(ctx, tx, ledger.Refund(re.ID, re.MerchantID, re.Livemode, re.Currency, re.Amount))
		return err
	})
	return re, err
}

func (s *Service) fail(ctx context.Context, id, reason string) (Refund, error) {
	return scan(s.pool.QueryRow(ctx, `
		UPDATE refunds SET status = 'failed', failure_reason = $2, updated_at = now()
		WHERE id = $1 AND status = 'pending' RETURNING `+columns, id, reason))
}

func (s *Service) Get(ctx context.Context, merchantID int64, livemode bool, id string) (Refund, error) {
	return scan(s.pool.QueryRow(ctx,
		`SELECT `+columns+` FROM refunds WHERE id = $1 AND merchant_id = $2 AND livemode = $3`, id, merchantID, livemode))
}

// List pages through refunds newest first, optionally only those of one PaymentIntent.
func (s *Service) List(ctx context.Context, merchantID int64, livemode bool, paymentIntentID string, p listing.Params) ([]Refund, bool, error) {
	query := `SELECT ` + columns + ` FROM refunds WHERE merchant_id = $1 AND livemode = $2`
	args := []any{merchantID, livemode}
	if paymentIntentID != "" {
		args = append(args, paymentIntentID)
		query += fmt.Sprintf(` AND payment_intent_id = $%d`, len(args))
	}
	var cursor Refund
	if id, _ := p.Cursor(); id != "" {
		var err error
		if cursor, err = s.Get(ctx, merchantID, livemode, id); err != nil {
			return nil, false, err
		}
	}
	query, args = p.Keyset(query, args, cursor.Created, cursor.ID)
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	page, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Refund, error) { return scan(row) })
	if err != nil {
		return nil, false, err
	}
	page, hasMore := listing.Trim(p, page)
	return page, hasMore, nil
}
