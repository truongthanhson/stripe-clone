// Package customer stores the people a merchant charges, so payments can be grouped by who paid.
package customer

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongthanhson/stripe-clone/internal/listing"
	"github.com/truongthanhson/stripe-clone/internal/metadata"
)

type Customer struct {
	ID          string
	MerchantID  int64
	Livemode    bool
	Email       *string
	Name        *string
	Description *string
	Metadata    map[string]string
	Created     time.Time
	Deleted     bool
}

// Params creates or updates a customer. Nil fields are left unchanged on update; an empty string
// clears a field, as in Stripe's API.
type Params struct {
	Email       *string
	Name        *string
	Description *string
	Metadata    *metadata.Update
}

var ErrNotFound = errors.New("customer not found")

// ValidationError names the parameter that is wrong.
type ValidationError struct {
	Param   string
	Message string
}

func (e *ValidationError) Error() string { return e.Param + ": " + e.Message }

func (p Params) validate() error {
	if p.Email != nil && *p.Email != "" {
		at := strings.Index(*p.Email, "@")
		if at < 1 || at == len(*p.Email)-1 || strings.ContainsAny(*p.Email, " \t\r\n") {
			return &ValidationError{"email", fmt.Sprintf("Invalid email address: %s", *p.Email)}
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

const columns = `id, merchant_id, livemode, email, name, description, metadata, created_at, deleted_at IS NOT NULL`

func scan(row pgx.Row) (Customer, error) {
	var c Customer
	err := row.Scan(&c.ID, &c.MerchantID, &c.Livemode, &c.Email, &c.Name, &c.Description, &c.Metadata, &c.Created, &c.Deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return Customer{}, ErrNotFound
	}
	return c, err
}

// nullIfEmpty turns "" into NULL, so clearing a field stores NULL rather than an empty string.
func nullIfEmpty(s *string) *string {
	if s != nil && *s == "" {
		return nil
	}
	return s
}

func (s *Store) Create(ctx context.Context, merchantID int64, livemode bool, p Params) (Customer, error) {
	if err := p.validate(); err != nil {
		return Customer{}, err
	}
	var md map[string]string
	var err error
	if p.Metadata != nil {
		if md, err = p.Metadata.Apply(nil); err != nil {
			return Customer{}, err
		}
	}
	if md == nil {
		md = map[string]string{}
	}
	return scan(s.pool.QueryRow(ctx, `
		INSERT INTO customers (id, merchant_id, livemode, email, name, description, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING `+columns,
		"cus_"+rand.Text(), merchantID, livemode, nullIfEmpty(p.Email), nullIfEmpty(p.Name), nullIfEmpty(p.Description), md))
}

// Get returns a customer of this merchant and mode. A deleted customer is returned with Deleted set,
// so the API can answer {"deleted": true} as Stripe does.
func (s *Store) Get(ctx context.Context, merchantID int64, livemode bool, id string) (Customer, error) {
	return scan(s.pool.QueryRow(ctx,
		`SELECT `+columns+` FROM customers WHERE id = $1 AND merchant_id = $2 AND livemode = $3`, id, merchantID, livemode))
}

func (s *Store) Update(ctx context.Context, merchantID int64, livemode bool, id string, p Params) (Customer, error) {
	if err := p.validate(); err != nil {
		return Customer{}, err
	}
	var updated Customer
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		c, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM customers
			WHERE id = $1 AND merchant_id = $2 AND livemode = $3 AND deleted_at IS NULL FOR UPDATE`, id, merchantID, livemode))
		if err != nil {
			return err
		}
		md := c.Metadata
		if p.Metadata != nil {
			if md, err = p.Metadata.Apply(c.Metadata); err != nil {
				return err
			}
		}
		// For each field: keep it when not sent, set it when sent, clear it when sent empty.
		updated, err = scan(tx.QueryRow(ctx, `
			UPDATE customers SET
				email       = CASE WHEN $2 THEN NULLIF($3, '') ELSE email END,
				name        = CASE WHEN $4 THEN NULLIF($5, '') ELSE name END,
				description = CASE WHEN $6 THEN NULLIF($7, '') ELSE description END,
				metadata = $8, updated_at = now()
			WHERE id = $1 RETURNING `+columns,
			id, p.Email != nil, deref(p.Email), p.Name != nil, deref(p.Name), p.Description != nil, deref(p.Description), md))
		return err
	})
	return updated, err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Delete hides the customer. Payments that reference it keep the reference.
func (s *Store) Delete(ctx context.Context, merchantID int64, livemode bool, id string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE customers SET deleted_at = now(), updated_at = now()
		WHERE id = $1 AND merchant_id = $2 AND livemode = $3 AND deleted_at IS NULL`, id, merchantID, livemode)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// List pages through live (not deleted) customers newest first, optionally only those with an email
// (compared case-insensitively).
func (s *Store) List(ctx context.Context, merchantID int64, livemode bool, email string, p listing.Params) ([]Customer, bool, error) {
	query := `SELECT ` + columns + ` FROM customers WHERE merchant_id = $1 AND livemode = $2 AND deleted_at IS NULL`
	args := []any{merchantID, livemode}
	if email != "" {
		args = append(args, email)
		query += fmt.Sprintf(` AND lower(email) = lower($%d)`, len(args))
	}
	var cursor Customer
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
	page, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Customer, error) { return scan(row) })
	if err != nil {
		return nil, false, err
	}
	page, hasMore := listing.Trim(p, page)
	return page, hasMore, nil
}
