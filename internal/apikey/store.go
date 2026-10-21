package apikey

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("api key not found or revoked")

// Principal is who a request acts as once its API key is verified.
type Principal struct {
	MerchantID int64
	Mode       Mode
}

func (p Principal) Livemode() bool { return p.Mode == ModeLive }

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) CreateMerchant(ctx context.Context, name, email string) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO merchants (name, email) VALUES ($1, $2) RETURNING id`,
		name, email,
	).Scan(&id)
	return id, err
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
		`SELECT merchant_id, mode FROM api_keys WHERE secret_hash = $1 AND revoked_at IS NULL`,
		Hash(secret),
	).Scan(&p.MerchantID, &mode)
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrNotFound
	}
	if err != nil {
		return Principal{}, err
	}
	p.Mode = Mode(mode)
	return p, nil
}
