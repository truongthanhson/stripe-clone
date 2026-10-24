package checkout

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongthanhson/stripe-clone/internal/listing"
	"github.com/truongthanhson/stripe-clone/internal/paymentintent"
)

var ErrNotFound = errors.New("checkout session not found")

// ErrTooManyAttempts means the session's card attempts are used up; the buyer needs a new session.
var ErrTooManyAttempts = errors.New("too many payment attempts for this checkout session")

type Service struct {
	pool    *pgxpool.Pool
	intents *paymentintent.Service
	now     func() time.Time
	hooks   []Hook
}

// Hook runs inside the transaction that completes or expires a session, exactly once per session.
// An error rolls the change back.
type Hook func(ctx context.Context, tx pgx.Tx, s Session) error

// OnChange registers a hook. Register hooks before serving requests; the list is not locked.
func (s *Service) OnChange(h Hook) {
	s.hooks = append(s.hooks, h)
}

func NewService(pool *pgxpool.Pool, intents *paymentintent.Service) *Service {
	return &Service{pool: pool, intents: intents, now: time.Now}
}

const columns = `s.id, s.merchant_id, s.merchant_name, s.livemode, s.status, s.payment_intent_id, s.currency, s.amount_total,
	s.line_items, s.success_url, s.cancel_url, s.customer_id, s.customer_email, s.customer_details_email,
	s.client_reference_id, s.metadata, s.expires_at, s.completed_at, s.created_at`

// from joins the merchant's name for the payment page; as a subquery, its columns stay unambiguous for
// listing.Keyset.
const from = ` FROM (SELECT cs.*, m.name AS merchant_name FROM checkout_sessions cs JOIN merchants m ON m.id = cs.merchant_id) s`

func scan(row pgx.Row) (Session, error) {
	var s Session
	err := row.Scan(&s.ID, &s.MerchantID, &s.MerchantName, &s.Livemode, &s.Status, &s.PaymentIntentID, &s.Currency,
		&s.AmountTotal, &s.LineItems, &s.SuccessURL, &s.CancelURL, &s.Customer, &s.CustomerEmail,
		&s.CustomerDetailsEmail, &s.ClientReferenceID, &s.Metadata, &s.ExpiresAt, &s.CompletedAt, &s.Created)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	return s, err
}

// Create stores the session and its PaymentIntent in one transaction, so there is never a session
// without a payment or an orphaned payment without a session.
func (s *Service) Create(ctx context.Context, merchantID int64, livemode bool, p CreateParams) (Session, error) {
	now := s.now()
	if err := p.validate(now); err != nil {
		return Session{}, err
	}
	md, err := p.Metadata.Apply(nil)
	if err != nil {
		return Session{}, err
	}
	expiresAt := now.Add(DefaultLifetime)
	if p.ExpiresAt != nil {
		expiresAt = *p.ExpiresAt
	}
	currency := p.LineItems[0].Currency

	var id string
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		pi, err := s.intents.CreateIn(ctx, tx, merchantID, livemode, paymentintent.CreateParams{
			Amount:        p.amountTotal(),
			Currency:      currency,
			CaptureMethod: paymentintent.CaptureAutomatic,
			Customer:      p.Customer,
		})
		if err != nil {
			return fromIntentError(err)
		}
		id = newID(livemode)
		_, err = tx.Exec(ctx, `
			INSERT INTO checkout_sessions (id, merchant_id, livemode, payment_intent_id, status, currency, amount_total,
				line_items, success_url, cancel_url, customer_id, customer_email, client_reference_id, metadata, expires_at)
			VALUES ($1, $2, $3, $4, 'open', $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
			id, merchantID, livemode, pi.ID, currency, pi.Amount, p.LineItems, p.SuccessURL, p.CancelURL,
			p.Customer, p.CustomerEmail, p.ClientReferenceID, md, expiresAt)
		return err
	})
	if err != nil {
		return Session{}, err
	}
	return s.Get(ctx, merchantID, livemode, id)
}

// fromIntentError renames the PaymentIntent's parameters to the ones the caller sent.
func fromIntentError(err error) error {
	var ve *paymentintent.ValidationError
	if !errors.As(err, &ve) {
		return err
	}
	switch ve.Param {
	case "amount":
		return &ValidationError{"line_items", ve.Code, "The total of line_items is out of range. " + ve.Message}
	case "currency":
		return &ValidationError{"line_items[0][price_data][currency]", ve.Code, ve.Message}
	}
	return &ValidationError{ve.Param, ve.Code, ve.Message}
}

// Get returns a session of this merchant and mode, brought up to date with its PaymentIntent.
func (s *Service) Get(ctx context.Context, merchantID int64, livemode bool, id string) (Session, error) {
	sess, err := scan(s.pool.QueryRow(ctx, `SELECT `+columns+from+` WHERE s.id = $1 AND s.merchant_id = $2 AND s.livemode = $3`,
		id, merchantID, livemode))
	if err != nil {
		return Session{}, err
	}
	return s.refresh(ctx, sess)
}

// Open returns a session by ID alone, for the public payment page: knowing the ID is the permission.
func (s *Service) Open(ctx context.Context, id string) (Session, error) {
	sess, err := scan(s.pool.QueryRow(ctx, `SELECT `+columns+from+` WHERE s.id = $1`, id))
	if err != nil {
		return Session{}, err
	}
	return s.refresh(ctx, sess)
}

func (s *Service) List(ctx context.Context, merchantID int64, livemode bool, p listing.Params) ([]Session, bool, error) {
	query := `SELECT ` + columns + from + ` WHERE s.merchant_id = $1 AND s.livemode = $2`
	args := []any{merchantID, livemode}
	var cursor Session
	if id, _ := p.Cursor(); id != "" {
		var err error
		if cursor, err = scan(s.pool.QueryRow(ctx, `SELECT `+columns+from+` WHERE s.id = $1 AND s.merchant_id = $2 AND s.livemode = $3`,
			id, merchantID, livemode)); err != nil {
			return nil, false, err
		}
	}
	query, args = p.Keyset(query, args, cursor.Created, cursor.ID)
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	page, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Session, error) { return scan(row) })
	if err != nil {
		return nil, false, err
	}
	page, hasMore := listing.Trim(p, page)
	for i := range page {
		if page[i], err = s.refresh(ctx, page[i]); err != nil {
			return nil, false, err
		}
	}
	return page, hasMore, nil
}

// Expire closes an open session so it can no longer be paid. It cancels the PaymentIntent first:
// if a payment already claimed it, the cancel loses that race and the session is not expired.
func (s *Service) Expire(ctx context.Context, merchantID int64, livemode bool, id string) (Session, error) {
	sess, err := s.Get(ctx, merchantID, livemode, id)
	if err != nil {
		return Session{}, err
	}
	if sess.Status != StatusOpen {
		return Session{}, &StateError{fmt.Sprintf("Only Checkout Sessions with a status of open can be expired; this one is %s.", sess.Status)}
	}
	if err := s.cancelIntent(ctx, sess); err != nil {
		return Session{}, err
	}
	sess, err = s.Get(ctx, merchantID, livemode, id)
	if err != nil {
		return Session{}, err
	}
	if sess.Status == StatusOpen {
		return Session{}, &StateError{"This Checkout Session has a payment in progress and cannot be expired."}
	}
	return sess, nil
}

// cancelIntent cancels the session's PaymentIntent unless a payment got to it first. Losing that
// race is not an error: the caller re-reads the session and sees how the payment went.
func (s *Service) cancelIntent(ctx context.Context, sess Session) error {
	reason := "abandoned"
	_, err := s.intents.Cancel(ctx, sess.MerchantID, sess.Livemode, sess.PaymentIntentID, &reason)
	var se *paymentintent.StateError
	if errors.As(err, &se) {
		return nil
	}
	return err
}

// Pay charges the buyer's card for an open session. A decline returns the PaymentIntent's
// DeclineError, and the session stays open for another card. Concurrent submits are safe: only one
// confirm claims the PaymentIntent, the others see it processing or succeeded.
func (s *Service) Pay(ctx context.Context, id, email, paymentMethod string) (Session, error) {
	sess, err := s.Open(ctx, id)
	if err != nil || sess.Status != StatusOpen {
		return sess, err
	}
	if sess.Intent.Attempts >= MaxAttempts {
		return sess, ErrTooManyAttempts
	}
	if email != "" {
		if _, err := s.pool.Exec(ctx, `UPDATE checkout_sessions SET customer_details_email = $2, updated_at = now()
			WHERE id = $1 AND status = 'open'`, id, email); err != nil {
			return sess, err
		}
	}
	_, payErr := s.intents.Confirm(ctx, sess.MerchantID, sess.Livemode, sess.PaymentIntentID, &paymentMethod)
	var se *paymentintent.StateError
	if errors.As(payErr, &se) {
		payErr = nil // another submit or an expiry got there first; the re-read shows which
	}
	sess, err = s.Open(ctx, id)
	if err != nil {
		return Session{}, err
	}
	return sess, payErr
}

// refresh loads the PaymentIntent and settles the session from it: succeeded means complete,
// canceled means expired. An open session past its expiry is expired here, lazily, the first time
// anyone looks at it.
func (s *Service) refresh(ctx context.Context, sess Session) (Session, error) {
	pi, err := s.intents.Get(ctx, sess.MerchantID, sess.Livemode, sess.PaymentIntentID)
	if err != nil {
		return Session{}, err
	}
	sess.Intent = pi
	if sess.Status != StatusOpen {
		return sess, nil
	}
	if s.now().After(sess.ExpiresAt) && pi.Status != paymentintent.StatusSucceeded && pi.Status != paymentintent.StatusCanceled {
		if err := s.cancelIntent(ctx, sess); err != nil {
			return Session{}, err
		}
		if sess.Intent, err = s.intents.Get(ctx, sess.MerchantID, sess.Livemode, sess.PaymentIntentID); err != nil {
			return Session{}, err
		}
	}
	if !settles(sess.Intent.Status) {
		return sess, nil
	}
	// IntentChanged normally settled the session already; this catches sessions whose payment ended
	// before that hook was registered.
	if err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error { return s.settle(ctx, tx, sess.Intent) }); err != nil {
		return Session{}, err
	}
	fresh, err := scan(s.pool.QueryRow(ctx, `SELECT `+columns+from+` WHERE s.id = $1`, sess.ID))
	if err != nil {
		return Session{}, err
	}
	fresh.Intent = sess.Intent
	return fresh, nil
}

func settles(st paymentintent.Status) bool {
	return st == paymentintent.StatusSucceeded || st == paymentintent.StatusCanceled
}

// IntentChanged is a paymentintent.Hook: when a session's PaymentIntent succeeds or is canceled, the
// session completes or expires in the same transaction.
func (s *Service) IntentChanged(ctx context.Context, tx pgx.Tx, pi paymentintent.PaymentIntent, _ paymentintent.Status) error {
	if !settles(pi.Status) {
		return nil
	}
	return s.settle(ctx, tx, pi)
}

// settle completes or expires the open session of a finished PaymentIntent. WHERE status = 'open'
// makes it happen once: concurrent callers match nothing, and only the one that changed the row
// runs the hooks.
func (s *Service) settle(ctx context.Context, tx pgx.Tx, pi paymentintent.PaymentIntent) error {
	to := StatusExpired
	if pi.Status == paymentintent.StatusSucceeded {
		to = StatusComplete
	}
	var id string
	err := tx.QueryRow(ctx, `UPDATE checkout_sessions
		SET status = $2::text, completed_at = CASE WHEN $2::text = 'complete' THEN now() END, updated_at = now()
		WHERE payment_intent_id = $1 AND status = 'open' RETURNING id`, pi.ID, string(to)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	sess, err := scan(tx.QueryRow(ctx, `SELECT `+columns+from+` WHERE s.id = $1`, id))
	if err != nil {
		return err
	}
	sess.Intent = pi
	for _, h := range s.hooks {
		if err := h(ctx, tx, sess); err != nil {
			return err
		}
	}
	return nil
}
