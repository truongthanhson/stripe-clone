// Package idempotency makes POST requests safe to retry: the first request with a given
// Idempotency-Key runs, and every retry within 24 hours gets the first response back.
package idempotency

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// TTL is how long a key and its response are kept, as Stripe does.
	TTL = 24 * time.Hour
	// LockTimeout is how long a request may hold a key before a retry may take it over, for the
	// case where the process died mid-request. It is well above the server's write timeout.
	LockTimeout  = time.Minute
	MaxKeyLength = 255
)

var (
	// ErrMismatch means the key was first used with a different method, path or parameters.
	ErrMismatch = errors.New("idempotency key reused with different request")
	// ErrInProgress means another request with the same key is still running.
	ErrInProgress = errors.New("idempotency key in use by a concurrent request")
)

// Scope identifies a key: keys are per merchant and per mode, so two merchants may both send "order-1".
type Scope struct {
	MerchantID int64
	Livemode   bool
	Key        string
}

type Response struct {
	Status int
	Body   []byte
}

// Fingerprint identifies "the same request": method, path and parameters. Parameters are
// re-encoded sorted by name, so a client that reorders them is still sending the same request.
func Fingerprint(method, path string, body []byte) []byte {
	canonical := string(body)
	if values, err := url.ParseQuery(canonical); err == nil {
		canonical = values.Encode()
	}
	sum := sha256.Sum256([]byte(method + " " + path + "\n" + canonical))
	return sum[:]
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Begin claims the key for this request. It returns (nil, nil) when the caller should run the
// request and then call Complete or Release; a stored Response when the caller should replay it;
// or ErrMismatch / ErrInProgress.
func (s *Store) Begin(ctx context.Context, sc Scope, fingerprint []byte) (*Response, error) {
	// A Release can delete the row between our INSERT and SELECT; one more round then wins.
	for range 3 {
		// The INSERT is the lock: the primary key lets exactly one concurrent request in.
		// An expired key may be reused for anything; an abandoned in-progress key only by the same request.
		tag, err := s.pool.Exec(ctx, `
			INSERT INTO idempotency_keys (merchant_id, livemode, key, fingerprint)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (merchant_id, livemode, key) DO UPDATE
			SET fingerprint = EXCLUDED.fingerprint, response_status = NULL, response_body = NULL,
			    created_at = now(), locked_at = now()
			WHERE idempotency_keys.created_at < now() - make_interval(secs => $5)
			   OR (idempotency_keys.response_status IS NULL
			       AND idempotency_keys.locked_at < now() - make_interval(secs => $6)
			       AND idempotency_keys.fingerprint = EXCLUDED.fingerprint)`,
			sc.MerchantID, sc.Livemode, sc.Key, fingerprint, TTL.Seconds(), LockTimeout.Seconds(),
		)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() == 1 {
			return nil, nil
		}

		var stored []byte
		var status *int
		var body []byte
		err = s.pool.QueryRow(ctx,
			`SELECT fingerprint, response_status, response_body FROM idempotency_keys
			 WHERE merchant_id = $1 AND livemode = $2 AND key = $3`,
			sc.MerchantID, sc.Livemode, sc.Key,
		).Scan(&stored, &status, &body)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		switch {
		case string(stored) != string(fingerprint):
			return nil, ErrMismatch
		case status == nil:
			return nil, ErrInProgress
		default:
			return &Response{Status: *status, Body: body}, nil
		}
	}
	return nil, ErrInProgress
}

// Complete stores the response so retries replay it.
func (s *Store) Complete(ctx context.Context, sc Scope, resp Response) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE idempotency_keys SET response_status = $4, response_body = $5
		 WHERE merchant_id = $1 AND livemode = $2 AND key = $3`,
		sc.MerchantID, sc.Livemode, sc.Key, resp.Status, resp.Body,
	)
	return err
}

// Release forgets the key, so a retry runs the request again.
func (s *Store) Release(ctx context.Context, sc Scope) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM idempotency_keys WHERE merchant_id = $1 AND livemode = $2 AND key = $3`,
		sc.MerchantID, sc.Livemode, sc.Key,
	)
	return err
}

// Prune deletes keys older than TTL and returns how many it removed.
func (s *Store) Prune(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM idempotency_keys WHERE created_at < now() - make_interval(secs => $1)`, TTL.Seconds())
	return tag.RowsAffected(), err
}
