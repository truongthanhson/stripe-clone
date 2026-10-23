package paymentintent

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound = errors.New("payment intent not found")
	// ErrStaleStatus means the PaymentIntent left the expected status before our update landed:
	// a concurrent request moved it first.
	ErrStaleStatus = errors.New("payment intent status changed concurrently")
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

const columns = `id, merchant_id, livemode, amount, currency, status, capture_method, description, payment_method, created_at,
	amount_capturable, amount_received, attempts, authorization_id, last_payment_error, canceled_at, cancellation_reason`

func scan(row pgx.Row) (PaymentIntent, error) {
	var pi PaymentIntent
	err := row.Scan(&pi.ID, &pi.MerchantID, &pi.Livemode, &pi.Amount, &pi.Currency, &pi.Status,
		&pi.CaptureMethod, &pi.Description, &pi.PaymentMethod, &pi.Created,
		&pi.AmountCapturable, &pi.AmountReceived, &pi.Attempts, &pi.AuthorizationID, &pi.LastPaymentError,
		&pi.CanceledAt, &pi.CancellationReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return PaymentIntent{}, ErrNotFound
	}
	return pi, err
}

// Create validates p and stores a new PaymentIntent owned by the merchant in the given mode.
func (s *Store) Create(ctx context.Context, merchantID int64, livemode bool, p CreateParams) (PaymentIntent, error) {
	if err := p.Validate(); err != nil {
		return PaymentIntent{}, err
	}
	return scan(s.pool.QueryRow(ctx, `
		INSERT INTO payment_intents (id, merchant_id, livemode, amount, currency, status, capture_method, description, payment_method)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING `+columns,
		NewID(), merchantID, livemode, p.Amount, p.Currency, p.InitialStatus(), p.CaptureMethod, p.Description, p.PaymentMethod,
	))
}

// Get returns the PaymentIntent only if it belongs to this merchant and mode; anything else is ErrNotFound,
// so a test key cannot even learn that a live PaymentIntent exists.
func (s *Store) Get(ctx context.Context, merchantID int64, livemode bool, id string) (PaymentIntent, error) {
	return scan(s.pool.QueryRow(ctx,
		`SELECT `+columns+` FROM payment_intents WHERE id = $1 AND merchant_id = $2 AND livemode = $3`,
		id, merchantID, livemode,
	))
}

// transition moves a PaymentIntent from one status to another and applies the extra assignments in
// set (which use $4 onwards). The WHERE status = from makes it a compare-and-swap: of two concurrent
// requests, exactly one matches the row, and the other gets ErrStaleStatus. No lock is held.
// With from == to it only updates fields, still asserting the status.
func (s *Store) transition(ctx context.Context, id string, from, to Status, set string, args ...any) (PaymentIntent, error) {
	if from != to {
		if err := CheckTransition(from, to); err != nil {
			return PaymentIntent{}, err
		}
	}
	if set != "" {
		set = ", " + set
	}
	pi, err := scan(s.pool.QueryRow(ctx,
		`UPDATE payment_intents SET status = $2, updated_at = now()`+set+`
		 WHERE id = $1 AND status = $3 RETURNING `+columns,
		append([]any{id, to, from}, args...)...,
	))
	if errors.Is(err, ErrNotFound) {
		return PaymentIntent{}, fmt.Errorf("%w: %s expected %s", ErrStaleStatus, id, from)
	}
	return pi, err
}
