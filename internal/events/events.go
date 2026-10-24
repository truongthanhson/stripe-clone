// Package events records what happened to a merchant's objects, for the events API and webhooks.
//
// Record runs inside the transaction of the change it describes (a transactional outbox): the event
// and the change commit together or not at all, so a webhook can never announce a payment that
// rolled back, and no committed payment can go unannounced.
package events

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongthanhson/stripe-clone/internal/apiversion"
	"github.com/truongthanhson/stripe-clone/internal/listing"
)

// Types are the event types a webhook endpoint can subscribe to, named as Stripe's.
var Types = []string{
	"checkout.session.completed",
	"checkout.session.expired",
	"payment_intent.amount_capturable_updated",
	"payment_intent.canceled",
	"payment_intent.created",
	"payment_intent.payment_failed",
	"payment_intent.processing",
	"payment_intent.succeeded",
	"refund.created",
	"refund.failed",
	"refund.updated",
}

// ValidType reports whether t is an event type, or "*" for all of them.
func ValidType(t string) bool {
	return t == "*" || slices.Contains(Types, t)
}

type Event struct {
	ID         string
	Seq        int64
	MerchantID int64
	Livemode   bool
	Type       string
	APIVersion string
	Data       json.RawMessage // the object, rendered as the API shows it in APIVersion
	Created    time.Time
	// PendingWebhooks counts deliveries of this event not yet acknowledged, as of the read.
	PendingWebhooks int
}

var ErrNotFound = errors.New("event not found")

// Record stores an event about object inside tx and queues a delivery to every endpoint of the
// merchant subscribed to typ. object is rendered as the API renders it, in the latest version; it
// is stored in the version the merchant is pinned to, as Stripe does.
//
// Events of one merchant are serialized by an advisory lock held until tx commits, so their seq
// order is their commit order and a reader never sees seq 6 before seq 5 exists. Callers record
// after their other writes, so the lock is the last one a transaction takes and cannot deadlock.
func Record(ctx context.Context, tx pgx.Tx, merchantID int64, livemode bool, typ string, object any) error {
	if !slices.Contains(Types, typ) {
		return fmt.Errorf("unknown event type %q", typ)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('events:' || $1::bigint, 0))`, merchantID); err != nil {
		return err
	}
	var version string
	if err := tx.QueryRow(ctx, `SELECT api_version FROM merchants WHERE id = $1`, merchantID).Scan(&version); err != nil {
		return err
	}
	data, err := renderAt(object, version)
	if err != nil {
		return err
	}
	id := "evt_" + rand.Text()
	if _, err := tx.Exec(ctx, `INSERT INTO events (id, merchant_id, livemode, type, api_version, data)
		VALUES ($1, $2, $3, $4, $5, $6)`, id, merchantID, livemode, typ, version, data); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO webhook_deliveries (event_id, endpoint_id)
		SELECT $1, id FROM webhook_endpoints
		WHERE merchant_id = $2 AND livemode = $3 AND deleted_at IS NULL AND status = 'enabled'
		  AND ($4 = ANY (enabled_events) OR '*' = ANY (enabled_events))`,
		id, merchantID, livemode, typ)
	return err
}

func renderAt(object any, version string) ([]byte, error) {
	b, err := json.Marshal(object)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	apiversion.Downgrade(v, version)
	return json.Marshal(v)
}

// JSON is an event as the API and webhooks show it.
type JSON struct {
	ID              string   `json:"id"`
	Object          string   `json:"object"`
	APIVersion      string   `json:"api_version"`
	Created         int64    `json:"created"`
	Data            DataJSON `json:"data"`
	Livemode        bool     `json:"livemode"`
	PendingWebhooks int      `json:"pending_webhooks"`
	Type            string   `json:"type"`
}

type DataJSON struct {
	Object json.RawMessage `json:"object"`
}

func (e Event) JSON() JSON {
	return JSON{ID: e.ID, Object: "event", APIVersion: e.APIVersion, Created: e.Created.Unix(),
		Data: DataJSON{Object: e.Data}, Livemode: e.Livemode, PendingWebhooks: e.PendingWebhooks, Type: e.Type}
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

const columns = `id, seq, merchant_id, livemode, type, api_version, data, created_at,
	(SELECT count(*) FROM webhook_deliveries d WHERE d.event_id = events.id AND d.status = 'pending')`

func scan(row pgx.Row) (Event, error) {
	var e Event
	err := row.Scan(&e.ID, &e.Seq, &e.MerchantID, &e.Livemode, &e.Type, &e.APIVersion, &e.Data, &e.Created, &e.PendingWebhooks)
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	return e, err
}

func (s *Store) Get(ctx context.Context, merchantID int64, livemode bool, id string) (Event, error) {
	return scan(s.pool.QueryRow(ctx, `SELECT `+columns+` FROM events WHERE id = $1 AND merchant_id = $2 AND livemode = $3`,
		id, merchantID, livemode))
}

// List pages through events newest first. typ filters by type: exact, or a prefix ending in ".*"
// such as "payment_intent.*".
func (s *Store) List(ctx context.Context, merchantID int64, livemode bool, typ string, p listing.Params) ([]Event, bool, error) {
	query := `SELECT ` + columns + ` FROM events WHERE merchant_id = $1 AND livemode = $2`
	args := []any{merchantID, livemode}
	if prefix, ok := strings.CutSuffix(typ, "*"); ok {
		args = append(args, prefix)
		query += fmt.Sprintf(` AND starts_with(type, $%d)`, len(args))
	} else if typ != "" {
		args = append(args, typ)
		query += fmt.Sprintf(` AND type = $%d`, len(args))
	}
	var cursor Event
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
	page, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) { return scan(row) })
	if err != nil {
		return nil, false, err
	}
	page, hasMore := listing.Trim(p, page)
	return page, hasMore, nil
}
