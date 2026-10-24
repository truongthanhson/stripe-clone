package webhook

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongthanhson/stripe-clone/internal/events"
	"github.com/truongthanhson/stripe-clone/internal/listing"
)

// MaxEndpoints per merchant and mode, as Stripe allows.
const MaxEndpoints = 16

type Endpoint struct {
	ID            string
	MerchantID    int64
	Livemode      bool
	URL           string
	Secret        string
	Description   *string
	EnabledEvents []string
	Status        string // enabled or disabled
	Created       time.Time
	Deleted       bool
}

// Params creates or updates an endpoint; nil fields are left unchanged on update.
type Params struct {
	URL           *string
	Description   *string
	EnabledEvents []string
	Disabled      *bool
}

var ErrNotFound = errors.New("webhook endpoint not found")

type ValidationError struct {
	Param   string
	Code    string
	Message string
}

func (e *ValidationError) Error() string { return e.Param + ": " + e.Message }

func (p Params) validate(livemode, creating bool) error {
	if creating && p.URL == nil {
		return &ValidationError{"url", "parameter_missing", "Missing required param: url."}
	}
	if p.URL != nil {
		u, err := url.Parse(*p.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || len(*p.URL) > 2048 {
			return &ValidationError{"url", "url_invalid", "Invalid URL: url must be an absolute http or https URL."}
		}
		if livemode && u.Scheme != "https" {
			return &ValidationError{"url", "url_invalid", "Live mode webhook endpoints must use https."}
		}
	}
	if creating && len(p.EnabledEvents) == 0 {
		return &ValidationError{"enabled_events", "parameter_missing", "Missing required param: enabled_events[]."}
	}
	for _, t := range p.EnabledEvents {
		if !events.ValidType(t) {
			return &ValidationError{"enabled_events", "parameter_invalid_string", fmt.Sprintf("Invalid event type: %q.", t)}
		}
	}
	return nil
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

const columns = `id, merchant_id, livemode, url, secret, description, enabled_events, status, created_at, deleted_at IS NOT NULL`

func scan(row pgx.Row) (Endpoint, error) {
	var e Endpoint
	err := row.Scan(&e.ID, &e.MerchantID, &e.Livemode, &e.URL, &e.Secret, &e.Description, &e.EnabledEvents, &e.Status, &e.Created, &e.Deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return Endpoint{}, ErrNotFound
	}
	return e, err
}

// Create registers an endpoint with a new signing secret. Only events recorded from now on reach it.
func (s *Store) Create(ctx context.Context, merchantID int64, livemode bool, p Params) (Endpoint, error) {
	if err := p.validate(livemode, true); err != nil {
		return Endpoint{}, err
	}
	var ep Endpoint
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Lock the merchant row so two concurrent creates cannot both pass the limit.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM merchants WHERE id = $1 FOR NO KEY UPDATE`, merchantID); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM webhook_endpoints
			WHERE merchant_id = $1 AND livemode = $2 AND deleted_at IS NULL`, merchantID, livemode).Scan(&n); err != nil {
			return err
		}
		if n >= MaxEndpoints {
			return &ValidationError{"url", "webhook_endpoints_limit", fmt.Sprintf("You can have at most %d webhook endpoints.", MaxEndpoints)}
		}
		var err error
		ep, err = scan(tx.QueryRow(ctx, `
			INSERT INTO webhook_endpoints (id, merchant_id, livemode, url, secret, description, enabled_events)
			VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7) RETURNING `+columns,
			"we_"+rand.Text(), merchantID, livemode, *p.URL, "whsec_"+rand.Text(), deref(p.Description), p.EnabledEvents))
		return err
	})
	return ep, err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (s *Store) Get(ctx context.Context, merchantID int64, livemode bool, id string) (Endpoint, error) {
	return scan(s.pool.QueryRow(ctx, `SELECT `+columns+` FROM webhook_endpoints
		WHERE id = $1 AND merchant_id = $2 AND livemode = $3 AND deleted_at IS NULL`, id, merchantID, livemode))
}

func (s *Store) Update(ctx context.Context, merchantID int64, livemode bool, id string, p Params) (Endpoint, error) {
	if err := p.validate(livemode, false); err != nil {
		return Endpoint{}, err
	}
	var status *string
	if p.Disabled != nil {
		st := "enabled"
		if *p.Disabled {
			st = "disabled"
		}
		status = &st
	}
	var enabled []string
	if len(p.EnabledEvents) > 0 {
		enabled = p.EnabledEvents
	}
	return scan(s.pool.QueryRow(ctx, `
		UPDATE webhook_endpoints SET
			url = COALESCE($4, url),
			description = CASE WHEN $5 THEN NULLIF($6, '') ELSE description END,
			enabled_events = COALESCE($7, enabled_events),
			status = COALESCE($8, status),
			updated_at = now()
		WHERE id = $1 AND merchant_id = $2 AND livemode = $3 AND deleted_at IS NULL RETURNING `+columns,
		id, merchantID, livemode, p.URL, p.Description != nil, deref(p.Description), enabled, status))
}

// Delete removes the endpoint; deliveries still queued for it are dropped as failed.
func (s *Store) Delete(ctx context.Context, merchantID int64, livemode bool, id string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE webhook_endpoints SET deleted_at = now(), updated_at = now()
			WHERE id = $1 AND merchant_id = $2 AND livemode = $3 AND deleted_at IS NULL`, id, merchantID, livemode)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		_, err = tx.Exec(ctx, `UPDATE webhook_deliveries SET status = 'failed', last_error = 'endpoint deleted', locked_until = NULL
			WHERE endpoint_id = $1 AND status = 'pending'`, id)
		return err
	})
}

func (s *Store) List(ctx context.Context, merchantID int64, livemode bool, p listing.Params) ([]Endpoint, bool, error) {
	query := `SELECT ` + columns + ` FROM webhook_endpoints WHERE merchant_id = $1 AND livemode = $2 AND deleted_at IS NULL`
	args := []any{merchantID, livemode}
	var cursor Endpoint
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
	page, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Endpoint, error) { return scan(row) })
	if err != nil {
		return nil, false, err
	}
	page, hasMore := listing.Trim(p, page)
	return page, hasMore, nil
}
