package apikey

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongthanhson/stripe-clone/internal/apiversion"
)

var ErrNotFound = errors.New("api key not found or revoked")

// Principal is who a request acts as once its API key is verified.
type Principal struct {
	MerchantID int64
	Mode       Mode
	APIVersion string // the version the merchant is pinned to
}

func (p Principal) Livemode() bool { return p.Mode == ModeLive }

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// CreateMerchant creates a merchant pinned to the latest API version.
func (s *Store) CreateMerchant(ctx context.Context, name, email string) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO merchants (name, email, api_version) VALUES ($1, $2, $3) RETURNING id`,
		name, email, apiversion.Latest(),
	).Scan(&id)
	return id, err
}

// SetAPIVersion re-pins a merchant, the step a merchant takes after testing their integration on a newer version.
func (s *Store) SetAPIVersion(ctx context.Context, merchantID int64, version string) error {
	if !apiversion.Valid(version) {
		return fmt.Errorf("unknown API version %q; released versions: %v", version, apiversion.All())
	}
	tag, err := s.pool.Exec(ctx, `UPDATE merchants SET api_version = $2, updated_at = now() WHERE id = $1`, merchantID, version)
	if err == nil && tag.RowsAffected() == 0 {
		err = fmt.Errorf("merchant %d not found", merchantID)
	}
	return err
}

func (s *Store) Issue(ctx context.Context, merchantID int64, mode Mode) (Key, error) {
	key, err := Generate(mode)
	if err != nil {
		return Key{}, err
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO api_keys (merchant_id, mode, prefix, secret_hash) VALUES ($1, $2, $3, $4)`,
		merchantID, string(key.Mode), key.Prefix, key.Hash,
	)
	if err != nil {
		return Key{}, err
	}
	return key, nil
}

func (s *Store) Lookup(ctx context.Context, secret string) (Principal, error) {
	var p Principal
	var mode string
	err := s.pool.QueryRow(ctx,
		`SELECT k.merchant_id, k.mode, m.api_version
		 FROM api_keys k JOIN merchants m ON m.id = k.merchant_id
		 WHERE k.secret_hash = $1 AND k.revoked_at IS NULL`,
		Hash(secret),
	).Scan(&p.MerchantID, &mode, &p.APIVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrNotFound
	}
	if err != nil {
		return Principal{}, err
	}
	p.Mode = Mode(mode)
	return p, nil
}

// Info describes a key without its secret, for listing.
type Info struct {
	ID        int64
	Mode      Mode
	Prefix    string
	Created   time.Time
	RevokedAt *time.Time
}

// List returns the merchant's keys in one mode, newest first, revoked ones included.
func (s *Store) List(ctx context.Context, merchantID int64, mode Mode) ([]Info, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, mode, prefix, created_at, revoked_at FROM api_keys
		WHERE merchant_id = $1 AND mode = $2 ORDER BY created_at DESC, id DESC`, merchantID, string(mode))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Info, error) {
		var k Info
		err := row.Scan(&k.ID, &k.Mode, &k.Prefix, &k.Created, &k.RevokedAt)
		return k, err
	})
}

// Revoke stops a key from authenticating, immediately and for good.
func (s *Store) Revoke(ctx context.Context, merchantID, keyID int64) error {
	tag, err := s.pool.Exec(ctx, `UPDATE api_keys SET revoked_at = now()
		WHERE id = $1 AND merchant_id = $2 AND revoked_at IS NULL`, keyID, merchantID)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}
