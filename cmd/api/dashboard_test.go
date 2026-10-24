package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/truongthanhson/stripe-clone/internal/dashboard"
)

// browser drives the dashboard like a person in a browser: it keeps cookies and sends back the
// CSRF token from the last page it loaded.
type browser struct {
	t       *testing.T
	h       http.Handler
	mu      sync.Mutex
	cookies map[string]*http.Cookie
	csrf    string
}

func newBrowser(t *testing.T, h http.Handler) *browser {
	return &browser{t: t, h: h, cookies: map[string]*http.Cookie{}}
}

var csrfField = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

func (b *browser) do(method, path string, form url.Values) *httptest.ResponseRecorder {
	b.t.Helper()
	var req *http.Request
	if form != nil {
		req = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	b.mu.Lock()
	for _, c := range b.cookies {
		req.AddCookie(c)
	}
	b.mu.Unlock()
	rec := httptest.NewRecorder()
	b.h.ServeHTTP(rec, req)

	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c
		}
	}
	if m := csrfField.FindStringSubmatch(rec.Body.String()); m != nil {
		b.csrf = m[1]
	}
	return rec
}

func (b *browser) get(path string) *httptest.ResponseRecorder {
	b.t.Helper()
	return b.do("GET", path, nil)
}

// submit posts a form with the CSRF token of the last page, as the page's own form would.
func (b *browser) submit(path string, form url.Values) *httptest.ResponseRecorder {
	b.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	if !form.Has("csrf_token") {
		b.mu.Lock()
		form.Set("csrf_token", b.csrf)
		b.mu.Unlock()
	}
	return b.do("POST", path, form)
}

func (b *browser) login(email, password string) *httptest.ResponseRecorder {
	b.t.Helper()
	b.get("/dashboard/login")
	return b.submit("/dashboard/login", url.Values{"email": {email}, "password": {password}})
}

func wantRedirect(t *testing.T, rec *httptest.ResponseRecorder, to string) {
	t.Helper()
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != to {
		t.Fatalf("got %d to %q, want 303 to %q\n%s", rec.Code, rec.Header().Get("Location"), to, rec.Body)
	}
}

func wantPage(t *testing.T, rec *httptest.ResponseRecorder, code int, contains ...string) {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("got %d, want %d\n%s", rec.Code, code, rec.Body)
	}
	for _, s := range contains {
		if !strings.Contains(rec.Body.String(), s) {
			t.Fatalf("page does not contain %q\n%s", s, rec.Body)
		}
	}
}

const testPassword = "correct horse battery"

// signedIn sets a password for the merchant behind e.testKey (Acme) and returns a signed-in browser.
func (e e2e) signedIn(t *testing.T) *browser {
	t.Helper()
	e.setPassword(t, "ops@acme.test")
	b := newBrowser(t, e.handler)
	wantRedirect(t, b.login("ops@acme.test", testPassword), "/dashboard/")
	return b
}

func (e e2e) setPassword(t *testing.T, email string) {
	t.Helper()
	ctx := context.Background()
	var id int64
	if err := e.pool.QueryRow(ctx, `SELECT id FROM merchants WHERE email = $1`, email).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := dashboard.NewStore(e.pool).SetPassword(ctx, id, testPassword); err != nil {
		t.Fatal(err)
	}
}

func TestE2EDashboardSignIn(t *testing.T) {
	e := newE2E(t)
	e.setPassword(t, "ops@acme.test")
	b := newBrowser(t, e.handler)

	wantRedirect(t, b.get("/dashboard/payments"), "/dashboard/login")
	wantPage(t, b.get("/dashboard/login"), http.StatusOK, "Sign in")

	wantPage(t, b.submit("/dashboard/login", url.Values{"email": {"ops@acme.test"}, "password": {"wrong password"}}),
		http.StatusUnauthorized, "Wrong email or password.")
	wantPage(t, b.submit("/dashboard/login", url.Values{"email": {"nobody@acme.test"}, "password": {testPassword}}),
		http.StatusUnauthorized, "Wrong email or password.")
	wantPage(t, b.submit("/dashboard/login", url.Values{"email": {"ops@acme.test"}, "password": {testPassword}, "csrf_token": {"forged"}}),
		http.StatusForbidden, "expired")

	rec := b.submit("/dashboard/login", url.Values{"email": {"OPS@acme.test"}, "password": {testPassword}})
	wantRedirect(t, rec, "/dashboard/")
	session := b.cookies[sessionCookie]
	if session == nil || !session.HttpOnly || !session.Secure || session.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie: %+v", session)
	}
	wantPage(t, b.get("/dashboard/"), http.StatusOK, "Acme", "Test mode: you are viewing test data")
	if csp := b.get("/dashboard/").Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("CSP: %q", csp)
	}

	stolen := *session
	wantRedirect(t, b.submit("/dashboard/logout", nil), "/dashboard/login")
	replay := newBrowser(t, e.handler)
	replay.cookies[sessionCookie] = &stolen
	wantRedirect(t, replay.get("/dashboard/"), "/dashboard/login")
}

func TestE2EDashboardLoginThrottle(t *testing.T) {
	e := newE2E(t)
	e.setPassword(t, "ops@acme.test")

	var wg sync.WaitGroup
	codes := make([]int, 30)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b := newBrowser(t, e.handler)
			codes[i] = b.login("ops@acme.test", "guess").Code
		}()
	}
	wg.Wait()
	checked := 0
	for _, c := range codes {
		if c == http.StatusUnauthorized {
			checked++
		}
	}
	if checked != dashboard.MaxLoginAttempts {
		t.Fatalf("%d of 30 concurrent guesses had their password checked, want %d", checked, dashboard.MaxLoginAttempts)
	}
	wantPage(t, newBrowser(t, e.handler).login("ops@acme.test", testPassword), http.StatusTooManyRequests, "Too many failed sign-ins")

	if _, err := e.pool.Exec(context.Background(), `UPDATE merchants SET login_window_started_at = now() - interval '16 minutes'`); err != nil {
		t.Fatal(err)
	}
	wantRedirect(t, newBrowser(t, e.handler).login("ops@acme.test", testPassword), "/dashboard/")
}

func TestE2EDashboardPayments(t *testing.T) {
	e := newE2E(t)
	b := e.signedIn(t)

	held := e.create(t, url.Values{"amount": {"2000"}, "capture_method": {"manual"}, "payment_method": {"pm_card_visa"}, "confirm": {"true"}})
	paid := e.create(t, url.Values{"amount": {"5000"}, "payment_method": {"pm_card_visa"}, "confirm": {"true"},
		"description": {"<b>Order</b> #1"}, "metadata[order_id]": {`"><script>x</script>`}})
	open := e.create(t, url.Values{"amount": {"700"}})
	_, other := e.call(t, "/v1/payment_intents", e.otherMerchantKey, url.Values{"amount": {"900"}, "currency": {"usd"}}, nil)

	t.Run("list and detail escape merchant data", func(t *testing.T) {
		wantPage(t, b.get("/dashboard/payments"), http.StatusOK, held.ID, paid.ID, open.ID, "&lt;b&gt;Order&lt;/b&gt; #1")
		rec := b.get("/dashboard/payments/" + paid.ID)
		wantPage(t, rec, http.StatusOK, "$50.00", "succeeded", "&lt;script&gt;")
		if strings.Contains(rec.Body.String(), "<script>x") {
			t.Fatal("metadata rendered unescaped")
		}
		wantPage(t, b.get("/dashboard/payments/"+other.ID), http.StatusNotFound, "No such payment")
	})

	t.Run("capture part of a hold", func(t *testing.T) {
		path := "/dashboard/payments/" + held.ID
		wantPage(t, b.get(path), http.StatusOK, "Capture", `value="20.00"`)
		wantPage(t, b.submit(path+"/capture", url.Values{"amount": {"abc"}}), http.StatusUnprocessableEntity, "Enter an amount like 10.50.")
		wantPage(t, b.submit(path+"/capture", url.Values{"amount": {"25"}}), http.StatusUnprocessableEntity, "at most the capturable amount")
		wantRedirect(t, b.submit(path+"/capture", url.Values{"amount": {"15.5"}}), path)
		wantPage(t, b.get(path), http.StatusOK, "Payment captured.", "$15.50")
		if _, pi := e.call(t, "/v1/payment_intents/"+held.ID+"/capture", e.testKey, url.Values{}, nil); pi.Error == nil {
			t.Fatalf("captured twice: %+v", pi)
		}
		wantPage(t, b.submit(path+"/capture", nil), http.StatusConflict, "status of succeeded")
	})

	t.Run("refund once however often the form is sent", func(t *testing.T) {
		path := "/dashboard/payments/" + paid.ID
		page := b.get(path).Body.String()
		token := regexp.MustCompile(`name="refund_token" value="([^"]+)"`).FindStringSubmatch(page)[1]
		var wg sync.WaitGroup
		for range 5 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				b.submit(path+"/refunds", url.Values{"amount": {"12.00"}, "reason": {"requested_by_customer"}, "refund_token": {token}})
			}()
		}
		wg.Wait()
		if n := e.count(t, `SELECT count(*) FROM refunds WHERE payment_intent_id = $1`, paid.ID); n != 1 {
			t.Fatalf("%d refunds, want 1", n)
		}
		wantPage(t, b.get(path), http.StatusOK, "$12.00", "requested_by_customer", `value="38.00"`)
		wantPage(t, b.submit(path+"/refunds", url.Values{"amount": {"38.01"}, "refund_token": {"t2"}}), http.StatusUnprocessableEntity, "refundable amount, 3800")
		wantRedirect(t, b.submit(path+"/refunds", url.Values{"refund_token": {"t3"}}), path)
		wantPage(t, b.get(path), http.StatusOK, "Refund sent.", "fully refunded")
	})

	t.Run("cancel", func(t *testing.T) {
		path := "/dashboard/payments/" + open.ID
		wantRedirect(t, b.submit(path+"/cancel", url.Values{"reason": {"requested_by_customer"}}), path)
		wantPage(t, b.get(path), http.StatusOK, "Payment canceled.", "canceled")
	})

	t.Run("a POST without the CSRF token changes nothing", func(t *testing.T) {
		pi := e.create(t, url.Values{"amount": {"1000"}, "capture_method": {"manual"}, "payment_method": {"pm_card_visa"}, "confirm": {"true"}})
		wantPage(t, b.submit("/dashboard/payments/"+pi.ID+"/capture", url.Values{"csrf_token": {""}}), http.StatusForbidden)
		wantPage(t, b.submit("/dashboard/payments/"+pi.ID+"/cancel", url.Values{"csrf_token": {"forged"}}), http.StatusForbidden)
		if code, body := e.do(t, "GET", "/v1/payment_intents/"+pi.ID, e.testKey, nil); code != 200 || body["status"] != "requires_capture" {
			t.Fatalf("after forged posts: %d %v", code, body)
		}
	})

	e.checkLedger(t)
}

func TestE2EDashboardPagesAndModes(t *testing.T) {
	e := newE2E(t)
	b := e.signedIn(t)

	code, cus := e.do(t, "POST", "/v1/customers", e.testKey, url.Values{"email": {"jenny@example.test"}, "name": {"Jenny Rosen"}})
	if code != 200 {
		t.Fatalf("customer: %d %v", code, cus)
	}
	var ids []string
	for range 30 {
		ids = append(ids, e.create(t, url.Values{"customer": {cus["id"].(string)}, "payment_method": {"pm_card_visa"}, "confirm": {"true"}}).ID)
	}

	rec := b.get("/dashboard/payments")
	wantPage(t, rec, http.StatusOK, ids[29], "Load more", "?starting_after="+ids[5])
	if strings.Contains(rec.Body.String(), ids[4]) {
		t.Fatal("first page has more than 25 payments")
	}
	rec = b.get("/dashboard/payments?starting_after=" + ids[5])
	wantPage(t, rec, http.StatusOK, ids[4], ids[0])
	if strings.Contains(rec.Body.String(), "Load more") {
		t.Fatal("last page offers more")
	}

	wantPage(t, b.get("/dashboard/customers"), http.StatusOK, "Jenny Rosen", "jenny@example.test")
	wantPage(t, b.get("/dashboard/customers/"+cus["id"].(string)), http.StatusOK, "Jenny Rosen", ids[29])
	wantPage(t, b.get("/dashboard/balance"), http.StatusOK, "usd pending", "charge", ids[29])
	wantPage(t, b.get("/dashboard/"), http.StatusOK, "usd pending", ids[29])

	_, live := e.call(t, "/v1/payment_intents", e.liveKey, url.Values{"amount": {"4200"}, "currency": {"eur"}}, nil)
	wantPage(t, b.get("/dashboard/payments/"+live.ID), http.StatusNotFound, "in test data")
	wantRedirect(t, b.submit("/dashboard/mode", url.Values{"livemode": {"true"}}), "/dashboard/")
	rec = b.get("/dashboard/payments")
	wantPage(t, rec, http.StatusOK, live.ID, "Switch to test data")
	if strings.Contains(rec.Body.String(), ids[29]) || strings.Contains(rec.Body.String(), "Test mode") {
		t.Fatal("live view shows test data")
	}
	wantPage(t, b.get("/dashboard/payments/"+live.ID), http.StatusOK, "€42.00")
}

func TestE2EDashboardAPIKeys(t *testing.T) {
	e := newE2E(t)
	b := e.signedIn(t)

	wantPage(t, b.get("/dashboard/apikeys"), http.StatusOK, "Create test key", e.testKey[:12]+"…")
	rec := b.submit("/dashboard/apikeys", nil)
	m := regexp.MustCompile(`<code class="key">(sk_test_[0-9a-f]+)</code>`).FindStringSubmatch(rec.Body.String())
	if rec.Code != http.StatusOK || m == nil {
		t.Fatalf("create key: %d\n%s", rec.Code, rec.Body)
	}
	secret := m[1]
	if code, _ := e.do(t, "GET", "/v1/account", secret, nil); code != 200 {
		t.Fatalf("new key: %d", code)
	}
	if strings.Contains(b.get("/dashboard/apikeys").Body.String(), secret) {
		t.Fatal("secret shown again")
	}

	var keyID string
	if err := e.pool.QueryRow(context.Background(), `SELECT id::text FROM api_keys WHERE prefix = $1 ORDER BY id DESC LIMIT 1`, secret[:12]).Scan(&keyID); err != nil {
		t.Fatal(err)
	}
	wantRedirect(t, b.submit("/dashboard/apikeys/"+keyID+"/revoke", nil), "/dashboard/apikeys")
	wantPage(t, b.get("/dashboard/apikeys"), http.StatusOK, "API key revoked.", "revoked")
	if code, _ := e.do(t, "GET", "/v1/account", secret, nil); code != 401 {
		t.Fatalf("revoked key: %d, want 401", code)
	}

	var otherKeyID string
	if err := e.pool.QueryRow(context.Background(), `SELECT id::text FROM api_keys WHERE prefix = $1`, e.otherMerchantKey[:12]).Scan(&otherKeyID); err != nil {
		t.Fatal(err)
	}
	wantPage(t, b.submit("/dashboard/apikeys/"+otherKeyID+"/revoke", nil), http.StatusNotFound)
	if code, _ := e.do(t, "GET", "/v1/account", e.otherMerchantKey, nil); code != 200 {
		t.Fatalf("another merchant's key was revoked: %d", code)
	}
}

func TestParseMajor(t *testing.T) {
	tests := []struct {
		in, currency string
		want         int64
		ok           bool
	}{
		{"12.50", "usd", 1250, true},
		{"12.5", "usd", 1250, true},
		{" 7 ", "usd", 700, true},
		{"0.01", "usd", 1, true},
		{"0.10", "usd", 10, true},
		{"500", "jpy", 500, true},
		{"5.5", "jpy", 0, false},
		{"1.234", "usd", 0, false},
		{"0", "usd", 0, false},
		{"-5", "usd", 0, false},
		{"1e3", "usd", 0, false},
		{"12.", "usd", 0, false},
		{"", "usd", 0, false},
		{"9999999999", "usd", 0, false},
	}
	for _, tt := range tests {
		got, err := parseMajor(tt.in, tt.currency)
		if (err == nil) != tt.ok || got != tt.want {
			t.Errorf("parseMajor(%q, %s) = %d, %v; want %d ok=%v", tt.in, tt.currency, got, err, tt.want, tt.ok)
		}
	}
}
