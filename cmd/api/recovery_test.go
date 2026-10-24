package main

import (
	"context"
	"math/rand/v2"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/truongthanhson/stripe-clone/internal/cardnetwork"
)

// lossyNetwork stands between the server and the bank and loses messages: a request that never
// arrives (the bank does nothing) or an answer that never comes back (the bank acted, we don't know).
// Either way the caller sees a timeout, which is all a real client sees.
type lossyNetwork struct {
	next cardnetwork.Network
	mu   sync.Mutex
	rnd  *rand.Rand
	loss float64 // probability of losing each message: the request, then the answer
	on   atomic.Bool
	lost atomic.Int64
}

func (l *lossyNetwork) lose() bool {
	if !l.on.Load() {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.rnd.Float64() < l.loss {
		l.lost.Add(1)
		return true
	}
	return false
}

func (l *lossyNetwork) wrap(call func() error) error {
	if l.lose() {
		return cardnetwork.ErrTimeout // request lost
	}
	if err := call(); err != nil || !l.lose() {
		return err
	}
	return cardnetwork.ErrTimeout // answer lost after the bank acted
}

func (l *lossyNetwork) Authorize(ctx context.Context, req cardnetwork.AuthorizeRequest) (cardnetwork.Authorization, error) {
	var auth cardnetwork.Authorization
	err := l.wrap(func() error {
		var err error
		auth, err = l.next.Authorize(ctx, req)
		return err
	})
	if err != nil {
		return cardnetwork.Authorization{}, err
	}
	return auth, nil
}

func (l *lossyNetwork) Capture(ctx context.Context, authorizationID string, amount int64) error {
	return l.wrap(func() error { return l.next.Capture(ctx, authorizationID, amount) })
}

func (l *lossyNetwork) Void(ctx context.Context, authorizationID string) error {
	return l.wrap(func() error { return l.next.Void(ctx, authorizationID) })
}

func (l *lossyNetwork) Refund(ctx context.Context, req cardnetwork.RefundRequest) error {
	return l.wrap(func() error { return l.next.Refund(ctx, req) })
}

// TestE2EPhase3Gate is the recovery gate: payments, captures, cancels and refunds run concurrently
// while 40% of the messages to and from the bank are lost. Afterwards the resolver, still behind the
// same lossy link, settles everything, and our books must agree with the bank to the cent: every
// hold the bank approved is captured, voided or still held exactly as our payment says, every
// refund the bank paid is recorded once, the ledger balances, and each outcome was announced once.
//
// The seed is logged; replay with GATE_SEED=<seed>. GATE_OPS sets the number of operations.
func TestE2EPhase3Gate(t *testing.T) {
	seed := uint64(time.Now().UnixNano())
	if s, err := strconv.ParseUint(os.Getenv("GATE_SEED"), 10, 64); err == nil {
		seed = s
	}
	ops := 400
	if n, err := strconv.Atoi(os.Getenv("GATE_OPS")); err == nil {
		ops = n
	}
	t.Logf("GATE_SEED=%d GATE_OPS=%d", seed, ops)

	var lossy *lossyNetwork
	e := newE2EWith(t, "", func(bank cardnetwork.Network) cardnetwork.Network {
		lossy = &lossyNetwork{next: bank, rnd: rand.New(rand.NewPCG(seed, 99)), loss: 0.4}
		return lossy
	})
	lossy.on.Store(true)

	cards := []string{"pm_card_visa", "pm_card_visa", "pm_card_visa", "pm_card_mastercard",
		"pm_card_chargeDeclined", "pm_card_refundFail", "pm_card_timeout"}
	var mu sync.Mutex
	var created, authorized, succeeded []string
	pick := func(r *rand.Rand, from *[]string) (string, bool) {
		mu.Lock()
		defer mu.Unlock()
		if len(*from) == 0 {
			return "", false
		}
		return (*from)[r.IntN(len(*from))], true
	}
	add := func(to *[]string, id string) {
		mu.Lock()
		*to = append(*to, id)
		mu.Unlock()
	}

	const workers = 8
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewPCG(seed, uint64(w)))
			for i := w; i < ops; i += workers {
				switch op := r.IntN(10); {
				case op < 5:
					form := url.Values{"amount": {strconv.Itoa(100 + r.IntN(9900))}, "currency": {"usd"},
						"payment_method": {cards[r.IntN(len(cards))]}, "confirm": {"true"}}
					if r.IntN(2) == 0 {
						form.Set("capture_method", "manual")
					}
					_, pi := e.call(t, "/v1/payment_intents", e.testKey, form, nil)
					if pi.ID == "" && pi.Error != nil && pi.Error.PaymentIntent != nil {
						pi.ID = pi.Error.PaymentIntent.ID
					}
					if pi.ID != "" {
						add(&created, pi.ID)
					}
					switch pi.Status {
					case "requires_capture":
						add(&authorized, pi.ID)
					case "succeeded":
						add(&succeeded, pi.ID)
					}
				case op < 7:
					if id, ok := pick(r, &authorized); ok {
						form := url.Values{}
						if r.IntN(2) == 0 {
							form.Set("amount_to_capture", "100")
						}
						if _, pi := e.call(t, "/v1/payment_intents/"+id+"/capture", e.testKey, form, nil); pi.Status == "succeeded" {
							add(&succeeded, id)
						}
					}
				case op < 8:
					if id, ok := pick(r, &authorized); ok {
						e.call(t, "/v1/payment_intents/"+id+"/cancel", e.testKey, url.Values{}, nil)
					}
				default:
					if id, ok := pick(r, &succeeded); ok {
						form := url.Values{"payment_intent": {id}}
						if r.IntN(2) == 0 {
							form.Set("amount", "50")
						}
						e.do(t, "POST", "/v1/refunds", e.testKey, form)
					}
				}
			}
		}()
	}
	wg.Wait()

	unknown := func() int {
		return e.count(t, `SELECT count(*) FROM payment_intents WHERE status = 'processing' OR unresolved_op IS NOT NULL`) +
			e.count(t, `SELECT count(*) FROM refunds WHERE status = 'pending'`)
	}
	t.Logf("%d payments created, %d bank messages lost, %d outcomes unknown after the run", len(created), lossy.lost.Load(), unknown())
	if unknown() == 0 {
		t.Fatal("nothing was left unknown; the test is not exercising the resolver")
	}

	// The resolver runs behind the same lossy link, so it may need several passes.
	for pass := 0; unknown() > 0; pass++ {
		if pass > 200 {
			t.Fatalf("%d outcomes still unknown after %d resolver passes", unknown(), pass)
		}
		if _, err := e.resolver.once(context.Background()); err != nil {
			t.Fatal(err)
		}
		if pass%10 == 9 {
			time.Sleep(e.resolver.grace) // lets anything never marked count as abandoned
		}
	}

	e.checkLedger(t)
	e.checkBooksMatchBank(t)
	e.checkEachOutcomeAnnouncedOnce(t)
}

// checkBooksMatchBank compares every authorization the bank holds with the PaymentIntent behind it.
func (e e2e) checkBooksMatchBank(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	type intent struct {
		status          string
		received        int64
		authorizationID *string
	}
	intents := map[string]intent{}
	rows, err := e.pool.Query(ctx, `SELECT id, status, amount_received, authorization_id FROM payment_intents`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		var pi intent
		if err := rows.Scan(&id, &pi.status, &pi.received, &pi.authorizationID); err != nil {
			t.Fatal(err)
		}
		intents[id] = pi
	}
	refunded := map[string]int64{}
	rows, err = e.pool.Query(ctx, `SELECT payment_intent_id, sum(amount) FROM refunds WHERE status = 'succeeded' GROUP BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		var sum int64
		if err := rows.Scan(&id, &sum); err != nil {
			t.Fatal(err)
		}
		refunded[id] = sum
	}

	var bankCaptured, booksReceived int64
	for _, h := range e.network.Holds() {
		if !h.Approved {
			continue
		}
		id, _, _ := strings.Cut(h.Reference, ":")
		pi, ok := intents[id]
		switch {
		case !ok:
			t.Errorf("bank holds %s for unknown payment %s", h.AuthorizationID, id)
			continue
		case pi.authorizationID == nil || *pi.authorizationID != h.AuthorizationID:
			t.Errorf("%s: bank approved %s, payment records %v (status %s)", id, h.AuthorizationID, pi.authorizationID, pi.status)
		case h.Captured > 0 && (pi.status != "succeeded" || pi.received != h.Captured):
			t.Errorf("%s: bank captured %d, payment is %s with %d received", id, h.Captured, pi.status, pi.received)
		case h.Voided && pi.status != "canceled":
			t.Errorf("%s: bank voided the hold, payment is %s", id, pi.status)
		case h.Captured == 0 && !h.Voided && pi.status != "requires_capture":
			t.Errorf("%s: bank still holds the money, payment is %s", id, pi.status)
		}
		if h.Refunded != refunded[id] {
			t.Errorf("%s: bank refunded %d, books say %d", id, h.Refunded, refunded[id])
		}
		bankCaptured += h.Captured
		if got := e.charged(t, id); got != h.Captured {
			t.Errorf("%s: ledger charged %d, bank captured %d", id, got, h.Captured)
		}
	}
	for _, pi := range intents {
		booksReceived += pi.received
	}
	if bankCaptured != booksReceived {
		t.Errorf("bank captured %d in total, payments received %d", bankCaptured, booksReceived)
	}
}

// checkEachOutcomeAnnouncedOnce: one succeeded event per succeeded payment, one per settled refund.
func (e e2e) checkEachOutcomeAnnouncedOnce(t *testing.T) {
	t.Helper()
	checks := []struct{ name, query string }{
		{"payment_intent.succeeded", `SELECT count(*) FROM payment_intents p WHERE status = 'succeeded' AND (SELECT count(*) FROM events
			WHERE type = 'payment_intent.succeeded' AND data->>'id' = p.id) <> 1`},
		{"payment_intent.canceled", `SELECT count(*) FROM payment_intents p WHERE status = 'canceled' AND (SELECT count(*) FROM events
			WHERE type = 'payment_intent.canceled' AND data->>'id' = p.id) <> 1`},
		{"refund.updated", `SELECT count(*) FROM refunds r WHERE status = 'succeeded' AND (SELECT count(*) FROM events
			WHERE type = 'refund.updated' AND data->>'id' = r.id) <> 1`},
		{"refund.failed", `SELECT count(*) FROM refunds r WHERE status = 'failed' AND (SELECT count(*) FROM events
			WHERE type = 'refund.failed' AND data->>'id' = r.id) <> 1`},
	}
	for _, c := range checks {
		if n := e.count(t, c.query); n != 0 {
			t.Errorf("%d objects without exactly one %s event", n, c.name)
		}
	}
}

// answerLoser lets the bank act but loses its answer while drop is set: the worst case, money moved
// and we don't know.
type answerLoser struct {
	cardnetwork.Network
	drop atomic.Bool
}

func (a *answerLoser) lost(err error) error {
	if err == nil && a.drop.Load() {
		return cardnetwork.ErrTimeout
	}
	return err
}

func (a *answerLoser) Capture(ctx context.Context, authorizationID string, amount int64) error {
	return a.lost(a.Network.Capture(ctx, authorizationID, amount))
}

func (a *answerLoser) Void(ctx context.Context, authorizationID string) error {
	return a.lost(a.Network.Void(ctx, authorizationID))
}

func (a *answerLoser) Refund(ctx context.Context, req cardnetwork.RefundRequest) error {
	return a.lost(a.Network.Refund(ctx, req))
}

func TestE2EUnknownOutcomes(t *testing.T) {
	var link *answerLoser
	e := newE2EWith(t, "", func(bank cardnetwork.Network) cardnetwork.Network {
		link = &answerLoser{Network: bank}
		return link
	})
	unresolved := func(id string) string {
		var op *string
		if err := e.pool.QueryRow(context.Background(), `SELECT unresolved_op FROM payment_intents WHERE id = $1`, id).Scan(&op); err != nil {
			t.Fatal(err)
		}
		if op == nil {
			return ""
		}
		return *op
	}
	resolve := func() {
		t.Helper()
		if _, err := e.resolver.once(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("an authorization whose answer never came is finished by the resolver", func(t *testing.T) {
		pi := e.create(t, url.Values{"payment_method": {"pm_card_timeout"}, "confirm": {"true"}})
		if pi.Status != "processing" || unresolved(pi.ID) != "authorize" {
			t.Fatalf("status %s, unresolved %q", pi.Status, unresolved(pi.ID))
		}
		resolve()
		if code, got := e.do(t, "GET", "/v1/payment_intents/"+pi.ID, e.testKey, nil); code != 200 || got["status"] != "succeeded" || unresolved(pi.ID) != "" {
			t.Fatalf("after resolving: %d %v", code, got)
		}
		if holds := e.network.Stats().Holds; holds != 1 {
			t.Fatalf("%d holds on the card, want 1", holds)
		}
	})

	t.Run("a capture whose answer was lost can only be repeated, then settles", func(t *testing.T) {
		pi := e.create(t, url.Values{"amount": {"3000"}, "capture_method": {"manual"}, "payment_method": {"pm_card_visa"}, "confirm": {"true"}})
		path := "/v1/payment_intents/" + pi.ID
		link.drop.Store(true)
		code, got := e.call(t, path+"/capture", e.testKey, url.Values{"amount_to_capture": {"2000"}}, nil)
		link.drop.Store(false)
		wantError(t, code, got, 502, "card_network_unavailable")
		if unresolved(pi.ID) != "capture" {
			t.Fatalf("unresolved = %q", unresolved(pi.ID))
		}
		code, got = e.call(t, path+"/capture", e.testKey, url.Values{"amount_to_capture": {"3000"}}, nil)
		wantError(t, code, got, 409, "payment_intent_outcome_pending")
		code, got = e.call(t, path+"/cancel", e.testKey, url.Values{}, nil)
		wantError(t, code, got, 409, "payment_intent_outcome_pending")
		resolve()
		if code, got := e.do(t, "GET", path, e.testKey, nil); code != 200 || got["status"] != "succeeded" || got["amount_received"] != 2000.0 {
			t.Fatalf("after resolving: %d %v", code, got)
		}
	})

	t.Run("a void whose answer was lost releases the hold", func(t *testing.T) {
		pi := e.create(t, url.Values{"capture_method": {"manual"}, "payment_method": {"pm_card_visa"}, "confirm": {"true"}})
		link.drop.Store(true)
		code, got := e.call(t, "/v1/payment_intents/"+pi.ID+"/cancel", e.testKey, url.Values{}, nil)
		link.drop.Store(false)
		wantError(t, code, got, 502, "card_network_unavailable")
		resolve()
		if code, got := e.do(t, "GET", "/v1/payment_intents/"+pi.ID, e.testKey, nil); code != 200 || got["status"] != "canceled" {
			t.Fatalf("after resolving: %d %v", code, got)
		}
	})

	t.Run("a refund whose answer was lost is recorded once", func(t *testing.T) {
		pi := e.create(t, url.Values{"payment_method": {"pm_card_visa"}, "confirm": {"true"}})
		link.drop.Store(true)
		code, re := e.refund(t, url.Values{"payment_intent": {pi.ID}, "amount": {"700"}}, nil)
		link.drop.Store(false)
		if code != 200 || re.Status != "pending" {
			t.Fatalf("refund: %d %+v", code, re)
		}
		resolve()
		resolve()
		if code, got := e.do(t, "GET", "/v1/refunds/"+re.ID, e.testKey, nil); code != 200 || got["status"] != "succeeded" {
			t.Fatalf("after resolving: %d %v", code, got)
		}
		if n := e.count(t, `SELECT count(*) FROM ledger_transactions WHERE type = 'refund' AND source_id = $1`, re.ID); n != 1 {
			t.Fatalf("%d ledger postings for the refund", n)
		}
	})
	e.checkLedger(t)
}
