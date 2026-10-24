package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongthanhson/stripe-clone/internal/events"
)

const (
	// RetryFor is how long a delivery is retried before it is given up as failed.
	RetryFor = 72 * time.Hour
	// lockFor is how long a claimed delivery belongs to one dispatcher; after that another may
	// take it over, which covers a dispatcher that died mid-send. It is well above the send timeout.
	lockFor = time.Minute
	// sendTimeout bounds one HTTP attempt, connect to last byte.
	sendTimeout = 10 * time.Second
)

// backoff is the wait after the n-th failed attempt (1-based); past its end the last value repeats.
var backoff = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 5 * time.Hour, 10 * time.Hour, 12 * time.Hour}

func retryAfter(attempts int) time.Duration {
	return backoff[min(attempts, len(backoff))-1]
}

// Dispatcher delivers queued events to webhook endpoints, at least once and in order per endpoint.
//
// Several dispatchers may run against one database (one per API process): a delivery is claimed with
// a conditional UPDATE, which only one of them can win.
type Dispatcher struct {
	pool   *pgxpool.Pool
	client *http.Client
	logger *slog.Logger
	now    func() time.Time
}

// NewDispatcher returns a dispatcher. Unless allowPrivate is set, it refuses to connect to loopback,
// private and link-local addresses, so a merchant cannot use a webhook URL to reach our own network.
func NewDispatcher(pool *pgxpool.Pool, logger *slog.Logger, allowPrivate bool) *Dispatcher {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	if !allowPrivate {
		dialer.Control = refusePrivate
	}
	return &Dispatcher{
		pool:   pool,
		logger: logger,
		now:    time.Now,
		client: &http.Client{
			Timeout: sendTimeout,
			Transport: &http.Transport{
				Proxy:               nil, // a proxy would dial on our behalf, past the address check
				DialContext:         dialer.DialContext,
				TLSHandshakeTimeout: 5 * time.Second,
				MaxIdleConnsPerHost: 2,
			},
			// A redirect is an answer, not a success: following it could lead to a private address.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// WithClock replaces the clock that schedules retries, so tests can move through days of retries.
// Signatures still use the real time, since receivers check them against their own clock.
func (d *Dispatcher) WithClock(now func() time.Time) *Dispatcher {
	d.now = now
	return d
}

// WithTimeout changes how long one attempt may take.
func (d *Dispatcher) WithTimeout(timeout time.Duration) *Dispatcher {
	d.client.Timeout = timeout
	return d
}

// refusePrivate runs after DNS resolution, on the address actually dialed, so a hostname that
// resolves to 127.0.0.1 is refused as well.
func refusePrivate(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || cgnat.Contains(ip) {
		return fmt.Errorf("refusing to connect to non-public address %s", host)
	}
	return nil
}

var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// Run delivers until ctx ends, polling for due deliveries.
func (d *Dispatcher) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		n, err := d.DeliverDue(ctx)
		if err != nil && ctx.Err() == nil {
			d.logger.Error("deliver webhooks", "error", err)
		}
		if n > 0 && err == nil {
			continue // there may be more queued behind what was just sent
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type claimed struct {
	id       int64
	attempts int
	url      string
	secret   string
	event    events.Event
}

// DeliverDue sends, for each endpoint, its oldest pending delivery if it is due, and records the
// outcomes. It returns how many it attempted.
//
// Only the oldest pending delivery of an endpoint is ever eligible, so an endpoint receives its
// events in the order they were recorded, and a later event never overtakes one being retried.
func (d *Dispatcher) DeliverDue(ctx context.Context) (int, error) {
	now := d.now()
	rows, err := d.pool.Query(ctx, `
		WITH heads AS (
			SELECT DISTINCT ON (endpoint_id) id FROM webhook_deliveries
			WHERE status = 'pending' ORDER BY endpoint_id, id
		)
		UPDATE webhook_deliveries d SET locked_until = $1::timestamptz + $2::int * interval '1 second', attempts = d.attempts + 1
		FROM heads, webhook_endpoints ep, events ev
		WHERE d.id = heads.id AND ep.id = d.endpoint_id AND ev.id = d.event_id
		  AND ep.status = 'enabled' AND ep.deleted_at IS NULL
		  AND d.status = 'pending' AND d.next_attempt_at <= $1 AND (d.locked_until IS NULL OR d.locked_until <= $1)
		RETURNING d.id, d.attempts, ep.url, ep.secret,
			ev.id, ev.seq, ev.merchant_id, ev.livemode, ev.type, ev.api_version, ev.data, ev.created_at`,
		now, int(lockFor.Seconds()))
	if err != nil {
		return 0, err
	}
	work, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (claimed, error) {
		var c claimed
		e := &c.event
		err := row.Scan(&c.id, &c.attempts, &c.url, &c.secret, &e.ID, &e.Seq, &e.MerchantID, &e.Livemode, &e.Type, &e.APIVersion, &e.Data, &e.Created)
		return c, err
	})
	if err != nil {
		return 0, err
	}

	var wg sync.WaitGroup
	for _, c := range work {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.deliver(ctx, c)
		}()
	}
	wg.Wait()
	return len(work), nil
}

func (d *Dispatcher) deliver(ctx context.Context, c claimed) {
	status, sendErr := d.send(ctx, c)
	// The answer must be recorded even if ctx was canceled meanwhile: forgetting a success means
	// sending again, forgetting a failure means waiting for the lock to expire.
	ctx = context.WithoutCancel(ctx)
	now := d.now()
	var err error
	if sendErr == nil {
		_, err = d.pool.Exec(ctx, `UPDATE webhook_deliveries SET status = 'succeeded', locked_until = NULL,
			last_attempt_at = $2, last_response_status = $3, last_error = NULL WHERE id = $1`, c.id, now, status)
	} else {
		next := now.Add(retryAfter(c.attempts))
		_, err = d.pool.Exec(ctx, `UPDATE webhook_deliveries SET
				status = CASE WHEN $2 > created_at + $5::int * interval '1 second' THEN 'failed' ELSE 'pending' END,
				next_attempt_at = $2, locked_until = NULL, last_attempt_at = $3, last_response_status = $4, last_error = $6
			WHERE id = $1`,
			c.id, next, now, nullIfZero(status), int(RetryFor.Seconds()), truncate(sendErr.Error(), 500))
	}
	if err != nil {
		d.logger.Error("record webhook delivery", "delivery", c.id, "error", err)
	}
}

// send POSTs the event and reports the response status. Anything but a 2xx is a failure.
func (d *Dispatcher) send(ctx context.Context, c claimed) (int, error) {
	var pending int
	if err := d.pool.QueryRow(ctx, `SELECT count(*) FROM webhook_deliveries WHERE event_id = $1 AND status = 'pending'`,
		c.event.ID).Scan(&pending); err != nil {
		return 0, err
	}
	c.event.PendingWebhooks = pending
	payload, err := json.Marshal(c.event.JSON())
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("User-Agent", "stripe-clone-webhooks/1.0")
	req.Header.Set(SignatureHeader, Sign(c.secret, time.Now(), payload))
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("endpoint answered %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

func nullIfZero(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
