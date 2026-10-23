package paymentintent

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongthanhson/stripe-clone/internal/ledger"
	"github.com/truongthanhson/stripe-clone/internal/listing"
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

const columns = `id, merchant_id, livemode, amount, currency, status, capture_method, description, payment_method, metadata, created_at,
	amount_capturable, amount_received, attempts, authorization_id, last_payment_error, canceled_at, cancellation_reason`

func scan(row pgx.Row) (PaymentIntent, error) {
	var pi PaymentIntent
	err := row.Scan(&pi.ID, &pi.MerchantID, &pi.Livemode, &pi.Amount, &pi.Currency, &pi.Status,
		&pi.CaptureMethod, &pi.Description, &pi.PaymentMethod, &pi.Metadata, &pi.Created,
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
	metadata, err := p.Metadata.Apply(nil)
	if err != nil {
		return PaymentIntent{}, err
	}
	return scan(s.pool.QueryRow(ctx, `
		INSERT INTO payment_intents (id, merchant_id, livemode, amount, currency, status, capture_method, description, payment_method, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING `+columns,
		NewID(), merchantID, livemode, p.Amount, p.Currency, p.InitialStatus(), p.CaptureMethod, p.Description, p.PaymentMethod, metadata,
	))
}

// Update changes description and metadata. The row is locked for the read-merge-write of metadata,
// so two concurrent updates of different keys both land; the transaction does no network I/O.
func (s *Store) Update(ctx context.Context, merchantID int64, livemode bool, id string, p UpdateParams) (PaymentIntent, error) {
	if err := validateDescription(p.Description); err != nil {
		return PaymentIntent{}, err
	}
	var updated PaymentIntent
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		pi, err := scan(tx.QueryRow(ctx,
			`SELECT `+columns+` FROM payment_intents WHERE id = $1 AND merchant_id = $2 AND livemode = $3 FOR UPDATE`,
			id, merchantID, livemode))
		if err != nil {
			return err
		}
		metadata := pi.Metadata
		if p.Metadata != nil {
			if metadata, err = p.Metadata.Apply(pi.Metadata); err != nil {
				return err
			}
		}
		updated, err = scan(tx.QueryRow(ctx,
			`UPDATE payment_intents SET description = COALESCE($2, description), metadata = $3, updated_at = now()
			 WHERE id = $1 RETURNING `+columns,
			id, p.Description, metadata))
		return err
	})
	return updated, err
}

// ListParams pages through PaymentIntents newest first.
type ListParams = listing.Params

// List returns up to Limit PaymentIntents and whether more exist beyond them in the paging direction.
func (s *Store) List(ctx context.Context, merchantID int64, livemode bool, p ListParams) ([]PaymentIntent, bool, error) {
	query := `SELECT ` + columns + ` FROM payment_intents WHERE merchant_id = $1 AND livemode = $2`
	args := []any{merchantID, livemode}
	var cursor PaymentIntent
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
	page, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (PaymentIntent, error) { return scan(row) })
	if err != nil {
		return nil, false, err
	}
	page, hasMore := listing.Trim(p, page)
	return page, hasMore, nil
}

// Get returns the PaymentIntent only if it belongs to this merchant and mode; anything else is ErrNotFound,
// so a test key cannot even learn that a live PaymentIntent exists.
func (s *Store) Get(ctx context.Context, merchantID int64, livemode bool, id string) (PaymentIntent, error) {
	return scan(s.pool.QueryRow(ctx,
		`SELECT `+columns+` FROM payment_intents WHERE id = $1 AND merchant_id = $2 AND livemode = $3`,
		id, merchantID, livemode,
	))
}

// querier is what both a pool and a transaction offer, so a query can run in either.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *Store) transition(ctx context.Context, id string, from, to Status, set string, args ...any) (PaymentIntent, error) {
	return transitionWith(ctx, s.pool, id, from, to, set, args...)
}

// succeed moves a PaymentIntent to succeeded and posts the charge to the ledger in one database
// transaction: either the money is recorded and the status says so, or neither happened.
func (s *Store) succeed(ctx context.Context, id string, from Status, set string, args ...any) (PaymentIntent, error) {
	var pi PaymentIntent
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		pi, err = transitionWith(ctx, tx, id, from, StatusSucceeded, set, args...)
		if err != nil {
			return err
		}
		_, err = ledger.Post(ctx, tx, ledger.Charge(pi.ID, pi.MerchantID, pi.Livemode, pi.Currency, pi.AmountReceived))
		return err
	})
	return pi, err
}

// transitionWith moves a PaymentIntent from one status to another and applies the extra assignments in
// set (which use $4 onwards). The WHERE status = from makes it a compare-and-swap: of two concurrent
// requests, exactly one matches the row, and the other gets ErrStaleStatus. No lock is held.
// With from == to it only updates fields, still asserting the status.
func transitionWith(ctx context.Context, q querier, id string, from, to Status, set string, args ...any) (PaymentIntent, error) {
	if from != to {
		if err := CheckTransition(from, to); err != nil {
			return PaymentIntent{}, err
		}
	}
	if set != "" {
		set = ", " + set
	}
	pi, err := scan(q.QueryRow(ctx,
		`UPDATE payment_intents SET status = $2, updated_at = now()`+set+`
		 WHERE id = $1 AND status = $3 RETURNING `+columns,
		append([]any{id, to, from}, args...)...,
	))
	if errors.Is(err, ErrNotFound) {
		return PaymentIntent{}, fmt.Errorf("%w: %s expected %s", ErrStaleStatus, id, from)
	}
	return pi, err
}
