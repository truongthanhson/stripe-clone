package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/truongthanhson/stripe-clone/internal/webhook"
)

// fakeClock is the time the dispatcher schedules retries by; tests move it instead of sleeping.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.After(c.t) {
		c.t = t
	}
}

// receiver is a merchant's webhook handler. It verifies signatures, and processes each event once
// by remembering the IDs it has seen, as the docs tell merchants to. failRate makes it flaky: it
// answers 500, drops the connection, or processes the event and then loses the answer.
type receiver struct {
	t        *testing.T
	secret   string
	mu       sync.Mutex
	rnd      *rand.Rand
	failRate float64
	hits     int
	seen     map[string]int // deliveries per event ID
	order    []string       // event IDs in the order they were first processed
	types    map[string]string
	badSigs  int
}

func newReceiver(t *testing.T, seed uint64, failRate float64) (*receiver, *httptest.Server) {
	r := &receiver{t: t, rnd: rand.New(rand.NewPCG(seed, 1)), failRate: failRate, seen: map[string]int{}, types: map[string]string{}}
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return r, srv
}

func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rc.mu.Lock()
	rc.hits++
	if err := webhook.Verify(body, r.Header.Get(webhook.SignatureHeader), rc.secret, webhook.DefaultTolerance, time.Now()); err != nil {
		rc.badSigs++
		rc.mu.Unlock()
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var ev struct{ ID, Type string }
	if err := json.Unmarshal(body, &ev); err != nil {
		rc.badSigs++
		rc.mu.Unlock()
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	roll := rc.rnd.Float64()
	process := func() {
		if rc.seen[ev.ID] == 0 {
			rc.order = append(rc.order, ev.ID)
			rc.types[ev.ID] = ev.Type
		}
		rc.seen[ev.ID]++
	}
	switch {
	case roll < rc.failRate*0.5:
		rc.mu.Unlock()
		http.Error(w, "busy", http.StatusInternalServerError)
	case roll < rc.failRate*0.75:
		rc.mu.Unlock()
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	case roll < rc.failRate:
		process() // processed, but the sender never hears so
		rc.mu.Unlock()
		http.Error(w, "lost answer", http.StatusServiceUnavailable)
	default:
		process()
		rc.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}
}

func (e e2e) createEndpoint(t *testing.T, key, url string, events ...string) (string, string) {
	t.Helper()
	form := map[string][]string{"url": {url}, "enabled_events[]": events}
	code, body := e.do(t, "POST", "/v1/webhook_endpoints", key, form)
	if code != 200 {
		t.Fatalf("create endpoint: %d %v", code, body)
	}
	return body["id"].(string), body["secret"].(string)
}

func (e e2e) dispatcher(clock *fakeClock, allowPrivate bool) *webhook.Dispatcher {
	return webhook.NewDispatcher(e.pool, slog.New(slog.NewTextHandler(io.Discard, nil)), allowPrivate).
		WithClock(clock.Now).WithTimeout(2 * time.Second)
}

// drain runs the dispatchers until nothing is pending, jumping the clock to the next retry whenever
// nothing is due. It fails the test if that takes more than limit of simulated time.
func (e e2e) drain(t *testing.T, clock *fakeClock, limit time.Duration, dispatchers ...*webhook.Dispatcher) {
	t.Helper()
	start := clock.Now()
	for i := 0; ; i++ {
		var wg sync.WaitGroup
		var mu sync.Mutex
		sent := 0
		for _, d := range dispatchers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				n, err := d.DeliverDue(context.Background())
				if err != nil {
					t.Error(err)
				}
				mu.Lock()
				sent += n
				mu.Unlock()
			}()
		}
		wg.Wait()
		if sent > 0 {
			continue
		}
		var next *time.Time
		// Only an endpoint's oldest pending delivery can be sent, so the next send is the earliest of those.
		if err := e.pool.QueryRow(context.Background(), `SELECT min(next_attempt_at) FROM (
			SELECT DISTINCT ON (endpoint_id) next_attempt_at FROM webhook_deliveries d
			JOIN webhook_endpoints ep ON ep.id = d.endpoint_id
			WHERE d.status = 'pending' AND ep.status = 'enabled' AND ep.deleted_at IS NULL
			ORDER BY endpoint_id, d.id) heads`).Scan(&next); err != nil {
			t.Fatal(err)
		}
		if next == nil {
			return
		}
		clock.Set(next.Add(time.Second))
		if clock.Now().Sub(start) > limit || i > 100_000 {
			t.Fatalf("deliveries still pending after %s of simulated time", clock.Now().Sub(start))
		}
	}
}

func TestE2EWebhookEndpoints(t *testing.T) {
	e := newE2E(t)

	id, secret := e.createEndpoint(t, e.testKey, "https://shop.example.test/hooks", "payment_intent.succeeded", "refund.created")
	if !strings.HasPrefix(id, "we_") || !strings.HasPrefix(secret, "whsec_") {
		t.Fatalf("endpoint %s secret %s", id, secret)
	}
	code, got := e.do(t, "GET", "/v1/webhook_endpoints/"+id, e.testKey, nil)
	if code != 200 || got["secret"] != nil || got["status"] != "enabled" || len(got["enabled_events"].([]any)) != 2 {
		t.Fatalf("get: %d %v", code, got)
	}
	if code, _ := e.do(t, "GET", "/v1/webhook_endpoints/"+id, e.otherMerchantKey, nil); code != 404 {
		t.Fatalf("other merchant: %d", code)
	}
	code, got = e.do(t, "POST", "/v1/webhook_endpoints/"+id, e.testKey, url.Values{"disabled": {"true"}, "enabled_events[0]": {"*"}, "description": {"orders"}})
	if code != 200 || got["status"] != "disabled" || got["enabled_events"].([]any)[0] != "*" || got["description"] != "orders" {
		t.Fatalf("update: %d %v", code, got)
	}
	code, list := e.do(t, "GET", "/v1/webhook_endpoints", e.testKey, nil)
	if code != 200 || len(list["data"].([]any)) != 1 {
		t.Fatalf("list: %d %v", code, list)
	}

	bad := []struct {
		name string
		key  string
		form url.Values
		code string
	}{
		{"unknown event", e.testKey, url.Values{"url": {"https://x.test"}, "enabled_events[]": {"payment_intent.exploded"}}, "parameter_invalid_string"},
		{"no events", e.testKey, url.Values{"url": {"https://x.test"}}, "parameter_missing"},
		{"not a URL", e.testKey, url.Values{"url": {"ftp://x.test"}, "enabled_events[]": {"*"}}, "url_invalid"},
		{"live needs https", e.liveKey, url.Values{"url": {"http://x.test"}, "enabled_events[]": {"*"}}, "url_invalid"},
		{"array sent as scalar", e.testKey, url.Values{"url": {"https://x.test"}, "enabled_events": {"*"}}, "parameter_invalid"},
	}
	for _, tt := range bad {
		code, body := e.do(t, "POST", "/v1/webhook_endpoints", tt.key, tt.form)
		if code != 400 || errorOf(body)["code"] != tt.code {
			t.Errorf("%s: %d %v, want 400 %s", tt.name, code, body, tt.code)
		}
	}

	code, del := e.do(t, "DELETE", "/v1/webhook_endpoints/"+id, e.testKey, nil)
	if code != 200 || del["deleted"] != true {
		t.Fatalf("delete: %d %v", code, del)
	}
	if code, _ := e.do(t, "GET", "/v1/webhook_endpoints/"+id, e.testKey, nil); code != 404 {
		t.Fatalf("get after delete: %d", code)
	}
}

func TestE2EEvents(t *testing.T) {
	e := newE2E(t)

	pi := e.create(t, url.Values{"payment_method": {"pm_card_visa"}, "confirm": {"true"}})
	_, declined := e.call(t, "/v1/payment_intents", e.testKey, url.Values{"amount": {"900"}, "currency": {"usd"},
		"payment_method": {"pm_card_chargeDeclined"}, "confirm": {"true"}}, nil)
	if code, body := e.do(t, "POST", "/v1/refunds", e.testKey, url.Values{"payment_intent": {pi.ID}, "amount": {"500"}}); code != 200 {
		t.Fatalf("refund: %d %v", code, body)
	}

	code, list := e.do(t, "GET", "/v1/events?limit=100", e.testKey, nil)
	if code != 200 {
		t.Fatalf("list: %d %v", code, list)
	}
	var types []string
	for _, ev := range list["data"].([]any) {
		types = append(types, ev.(map[string]any)["type"].(string))
	}
	slices.Reverse(types)
	want := []string{"payment_intent.created", "payment_intent.processing", "payment_intent.succeeded",
		"payment_intent.created", "payment_intent.processing", "payment_intent.payment_failed",
		"refund.created", "refund.updated"}
	if !slices.Equal(types, want) {
		t.Fatalf("events %v\nwant   %v", types, want)
	}

	first := list["data"].([]any)[0].(map[string]any)
	code, got := e.do(t, "GET", "/v1/events/"+first["id"].(string), e.testKey, nil)
	obj := got["data"].(map[string]any)["object"].(map[string]any)
	if code != 200 || got["type"] != "refund.updated" || obj["object"] != "refund" || obj["status"] != "succeeded" || got["api_version"] == "" {
		t.Fatalf("get: %d %v", code, got)
	}

	code, failed := e.do(t, "GET", "/v1/events?type=payment_intent.payment_failed", e.testKey, nil)
	data := failed["data"].([]any)
	if code != 200 || len(data) != 1 {
		t.Fatalf("filter: %d %v", code, failed)
	}
	if fo := data[0].(map[string]any)["data"].(map[string]any)["object"].(map[string]any); fo["id"] != declined.Error.PaymentIntent.ID ||
		fo["last_payment_error"].(map[string]any)["decline_code"] != "generic_decline" {
		t.Fatalf("payment_failed object: %v", fo)
	}
	if code, group := e.do(t, "GET", "/v1/events?type=refund.*", e.testKey, nil); code != 200 || len(group["data"].([]any)) != 2 {
		t.Fatalf("group filter: %d %v", code, group)
	}
	if code, other := e.do(t, "GET", "/v1/events", e.otherMerchantKey, nil); code != 200 || len(other["data"].([]any)) != 0 {
		t.Fatalf("other merchant sees events: %v", other)
	}
	if code, live := e.do(t, "GET", "/v1/events", e.liveKey, nil); code != 200 || len(live["data"].([]any)) != 0 {
		t.Fatalf("live mode sees test events: %v", live)
	}

	// An event is stored in the shape of the version the merchant is pinned to.
	if _, err := e.pool.Exec(context.Background(), `UPDATE merchants SET api_version = '2026-10-01'`); err != nil {
		t.Fatal(err)
	}
	e.create(t, url.Values{"capture_method": {"automatic_async"}})
	code, latest := e.do(t, "GET", "/v1/events?limit=1", e.testKey, nil)
	ev := latest["data"].([]any)[0].(map[string]any)
	if code != 200 || ev["api_version"] != "2026-10-01" || ev["data"].(map[string]any)["object"].(map[string]any)["capture_method"] != "automatic" {
		t.Fatalf("old-version event: %v", ev)
	}
}

// TestE2EWebhooksGate is the webhook delivery gate: hundreds of events reach a receiver that fails
// about a third of the time (errors, dropped connections, answers lost after processing), through
// two dispatchers racing each other. Every event must be processed exactly once by ID, in the order
// the events happened, with every signature valid. The seed is logged; replay with GATE_SEED=<seed>.
func TestE2EWebhooksGate(t *testing.T) {
	seed := uint64(time.Now().UnixNano())
	if s, err := strconv.ParseUint(os.Getenv("GATE_SEED"), 10, 64); err == nil {
		seed = s
	}
	t.Logf("GATE_SEED=%d", seed)
	e := newE2E(t)
	clock := &fakeClock{t: time.Now().Add(time.Minute)}

	flaky, flakySrv := newReceiver(t, seed, 0.35)
	steady, steadySrv := newReceiver(t, seed+1, 0)
	off, offSrv := newReceiver(t, seed+2, 0)
	var flakyID, offID string
	flakyID, flaky.secret = e.createEndpoint(t, e.testKey, flakySrv.URL+"/hooks", "*")
	_, steady.secret = e.createEndpoint(t, e.testKey, steadySrv.URL+"/hooks", "payment_intent.succeeded")
	offID, off.secret = e.createEndpoint(t, e.testKey, offSrv.URL+"/hooks", "*")
	if code, _ := e.do(t, "POST", "/v1/webhook_endpoints/"+offID, e.testKey, url.Values{"disabled": {"true"}}); code != 200 {
		t.Fatal("disable endpoint")
	}

	r := rand.New(rand.NewPCG(seed, 2))
	cards := []string{"pm_card_visa", "pm_card_visa", "pm_card_mastercard", "pm_card_chargeDeclined", "pm_card_refundFail"}
	for i := 0; i < 50; i++ {
		form := url.Values{"amount": {strconv.Itoa(500 + r.IntN(5000))}, "currency": {"usd"}, "payment_method": {cards[r.IntN(len(cards))]}, "confirm": {"true"}}
		if r.IntN(3) == 0 {
			form.Set("capture_method", "manual")
		}
		code, pi := e.call(t, "/v1/payment_intents", e.testKey, form, nil)
		if code == 200 && pi.Status == "requires_capture" {
			if r.IntN(2) == 0 {
				e.call(t, "/v1/payment_intents/"+pi.ID+"/capture", e.testKey, url.Values{}, nil)
			} else {
				e.call(t, "/v1/payment_intents/"+pi.ID+"/cancel", e.testKey, url.Values{}, nil)
			}
		}
		if code == 200 && r.IntN(3) == 0 {
			e.do(t, "POST", "/v1/refunds", e.testKey, url.Values{"payment_intent": {pi.ID}})
		}
	}
	other, otherSrv := newReceiver(t, seed+3, 0)
	_, other.secret = e.createEndpoint(t, e.otherMerchantKey, otherSrv.URL, "payment_intent.created")
	e.do(t, "POST", "/v1/payment_intents", e.otherMerchantKey, url.Values{"amount": {"700"}, "currency": {"usd"}})

	e.drain(t, clock, webhook.RetryFor, e.dispatcher(clock, true), e.dispatcher(clock, true))

	var want []string
	rows, err := e.pool.Query(context.Background(), `SELECT ev.id FROM webhook_deliveries d JOIN events ev ON ev.id = d.event_id
		WHERE d.endpoint_id = $1 ORDER BY ev.seq`, flakyID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		want = append(want, id)
	}
	if len(want) < 150 {
		t.Fatalf("only %d events were recorded", len(want))
	}
	if !slices.Equal(flaky.order, want) {
		t.Fatalf("flaky receiver processed %d events, want all %d in event order", len(flaky.order), len(want))
	}
	redelivered := 0
	for _, n := range flaky.seen {
		redelivered += n - 1
	}
	if flaky.badSigs > 0 || steady.badSigs > 0 {
		t.Fatalf("bad signatures: flaky %d, steady %d", flaky.badSigs, steady.badSigs)
	}
	if redelivered == 0 || flaky.hits <= len(want) {
		t.Fatalf("receiver never saw a redelivery (%d hits for %d events); the test is not exercising retries", flaky.hits, len(want))
	}
	t.Logf("%d events, %d requests to the flaky receiver, %d processed-then-redelivered", len(want), flaky.hits, redelivered)

	for _, typ := range steady.types {
		if typ != "payment_intent.succeeded" {
			t.Fatalf("steady receiver got %s", typ)
		}
	}
	succeeded := e.count(t, `SELECT count(*) FROM events WHERE type = 'payment_intent.succeeded' AND merchant_id =
		(SELECT merchant_id FROM webhook_endpoints WHERE id = $1)`, flakyID)
	if len(steady.order) != succeeded || steady.hits != succeeded {
		t.Fatalf("steady receiver: %d events in %d requests, want %d succeeded events (and nothing of the other merchant's)", len(steady.order), steady.hits, succeeded)
	}
	if len(other.order) != 1 || other.badSigs != 0 {
		t.Fatalf("the other merchant's endpoint got %d events (%d bad signatures), want its 1", len(other.order), other.badSigs)
	}
	if off.hits != 0 {
		t.Fatalf("disabled endpoint received %d requests", off.hits)
	}
	if n := e.count(t, `SELECT count(*) FROM webhook_deliveries WHERE status <> 'succeeded' AND endpoint_id <> $1`, offID); n != 0 {
		t.Fatalf("%d deliveries did not succeed", n)
	}
}

func TestE2EWebhookRetriesEnd(t *testing.T) {
	e := newE2E(t)
	clock := &fakeClock{t: time.Now().Add(time.Minute)}
	hits := 0
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	id, _ := e.createEndpoint(t, e.testKey, srv.URL, "payment_intent.created")
	e.create(t, url.Values{})

	e.drain(t, clock, webhook.RetryFor+24*time.Hour, e.dispatcher(clock, true))

	var status string
	var attempts, lastStatus int
	if err := e.pool.QueryRow(context.Background(), `SELECT status, attempts, last_response_status FROM webhook_deliveries
		WHERE endpoint_id = $1`, id).Scan(&status, &attempts, &lastStatus); err != nil {
		t.Fatal(err)
	}
	// 1m, 5m, 30m, 2h, 5h, 10h, then every 12h until 72h have passed.
	if status != "failed" || attempts != 11 || lastStatus != 500 || hits != attempts {
		t.Fatalf("status %s after %d attempts (%d requests), last answer %d; want failed after 11", status, attempts, hits, lastStatus)
	}
}

func TestE2EWebhooksRefusePrivateAddresses(t *testing.T) {
	e := newE2E(t)
	clock := &fakeClock{t: time.Now().Add(time.Minute)}
	rc, srv := newReceiver(t, 1, 0)
	id, secret := e.createEndpoint(t, e.testKey, srv.URL, "*")
	rc.secret = secret
	e.create(t, url.Values{})

	if _, err := e.dispatcher(clock, false).DeliverDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	var lastError string
	if err := e.pool.QueryRow(context.Background(), `SELECT last_error FROM webhook_deliveries WHERE endpoint_id = $1`, id).Scan(&lastError); err != nil {
		t.Fatal(err)
	}
	if rc.hits != 0 || !strings.Contains(lastError, "non-public address") {
		t.Fatalf("%d requests reached 127.0.0.1; last error %q", rc.hits, lastError)
	}
}
