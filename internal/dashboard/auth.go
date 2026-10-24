// Package dashboard signs merchants in to the web dashboard: passwords, login throttling and
// browser sessions. The pages themselves live in cmd/api.
package dashboard

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

const (
	// SessionLifetime is how long a sign-in lasts; after that the merchant signs in again.
	SessionLifetime = 12 * time.Hour
	// MaxLoginAttempts failed passwords within LoginWindow lock the account until the window ends.
	MaxLoginAttempts = 5
	LoginWindow      = 15 * time.Minute
	MinPasswordLen   = 8
)

var (
	ErrBadCredentials = errors.New("wrong email or password")
	ErrTooManyLogins  = errors.New("too many failed sign-ins; try again later")
	ErrNoSession      = errors.New("no such session or it expired")
	ErrWeakPassword   = errors.New("password must be at least 8 characters")
)

// Session is one signed-in browser.
type Session struct {
	MerchantID   int64
	MerchantName string
	Livemode     bool
	CSRFToken    string
	tokenHash    []byte
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// SetPassword sets the merchant's dashboard password and clears any lockout.
func (s *Store) SetPassword(ctx context.Context, merchantID int64, password string) error {
	if len(password) < MinPasswordLen {
		return ErrWeakPassword
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE merchants SET password_hash = $2, login_attempts = 0, updated_at = now() WHERE id = $1`,
		merchantID, string(hash))
	if err == nil && tag.RowsAffected() == 0 {
		err = errors.New("merchant not found")
	}
	return err
}

// dummyHash is compared against when the email is unknown, so a wrong email takes as long as a
// wrong password and the response time does not reveal which emails have accounts.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("not a real password"), bcrypt.DefaultCost)

// Login checks an email and password and returns the merchant's ID.
//
// The attempt is counted before the password is checked, in one conditional UPDATE: of many
// concurrent guesses, only MaxLoginAttempts per window get a password check at all. A correct
// password resets the count.
func (s *Store) Login(ctx context.Context, email, password string) (int64, error) {
	var id int64
	var hash string
	err := s.pool.QueryRow(ctx, `
		UPDATE merchants SET
			login_attempts = CASE WHEN login_window_started_at > now() - $2::int * interval '1 second' THEN login_attempts + 1 ELSE 1 END,
			login_window_started_at = CASE WHEN login_window_started_at > now() - $2::int * interval '1 second' THEN login_window_started_at ELSE now() END
		WHERE lower(email) = lower($1) AND password_hash IS NOT NULL
		  AND (login_attempts < $3 OR login_window_started_at <= now() - $2::int * interval '1 second')
		RETURNING id, password_hash`,
		email, int(LoginWindow.Seconds()), MaxLoginAttempts,
	).Scan(&id, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		var locked bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM merchants
			WHERE lower(email) = lower($1) AND password_hash IS NOT NULL)`, email).Scan(&locked); err != nil {
			return 0, err
		}
		if locked {
			return 0, ErrTooManyLogins
		}
		return 0, ErrBadCredentials
	}
	if err != nil {
		return 0, err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return 0, ErrBadCredentials
	}
	_, err = s.pool.Exec(ctx, `UPDATE merchants SET login_attempts = 0, login_window_started_at = NULL WHERE id = $1`, id)
	return id, err
}

// CreateSession signs a browser in. The returned token goes in its cookie; it is shown nowhere else.
func (s *Store) CreateSession(ctx context.Context, merchantID int64) (string, error) {
	token := rand.Text()
	_, err := s.pool.Exec(ctx, `
		INSERT INTO dashboard_sessions (token_hash, merchant_id, csrf_token, expires_at)
		VALUES ($1, $2, $3, now() + $4::int * interval '1 second')`,
		hashToken(token), merchantID, rand.Text(), int(SessionLifetime.Seconds()))
	if err != nil {
		return "", err
	}
	// Expired rows are only ever read as "no session"; clearing them here keeps the table small.
	_, err = s.pool.Exec(ctx, `DELETE FROM dashboard_sessions WHERE expires_at < now()`)
	return token, err
}

// Session returns the signed-in session for a cookie token.
func (s *Store) Session(ctx context.Context, token string) (Session, error) {
	sess := Session{tokenHash: hashToken(token)}
	err := s.pool.QueryRow(ctx, `
		SELECT s.merchant_id, m.name, s.livemode, s.csrf_token
		FROM dashboard_sessions s JOIN merchants m ON m.id = s.merchant_id
		WHERE s.token_hash = $1 AND s.expires_at > now()`, sess.tokenHash,
	).Scan(&sess.MerchantID, &sess.MerchantName, &sess.Livemode, &sess.CSRFToken)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNoSession
	}
	return sess, err
}

// SetLivemode switches which data the session shows: test or live.
func (s *Store) SetLivemode(ctx context.Context, sess Session, livemode bool) error {
	_, err := s.pool.Exec(ctx, `UPDATE dashboard_sessions SET livemode = $2 WHERE token_hash = $1`, sess.tokenHash, livemode)
	return err
}

// DeleteSession signs the browser out.
func (s *Store) DeleteSession(ctx context.Context, sess Session) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM dashboard_sessions WHERE token_hash = $1`, sess.tokenHash)
	return err
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// NewToken returns a random token for a one-off form value, such as the sign-in form's CSRF token.
func NewToken() string {
	return rand.Text()
}
