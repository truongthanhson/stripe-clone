package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/truongthanhson/stripe-clone/internal/apikey"
	"github.com/truongthanhson/stripe-clone/internal/dashboard"
)

type dashboardAuth interface {
	Login(ctx context.Context, email, password string) (int64, error)
	CreateSession(ctx context.Context, merchantID int64) (string, error)
	Session(ctx context.Context, token string) (dashboard.Session, error)
	SetLivemode(ctx context.Context, sess dashboard.Session, livemode bool) error
	DeleteSession(ctx context.Context, sess dashboard.Session) error
}

type keyManager interface {
	List(ctx context.Context, merchantID int64, mode apikey.Mode) ([]apikey.Info, error)
	Issue(ctx context.Context, merchantID int64, mode apikey.Mode) (apikey.Key, error)
	Revoke(ctx context.Context, merchantID, keyID int64) error
}

const (
	sessionCookie = "dashboard_session"
	loginCookie   = "dashboard_login" // CSRF token for the sign-in form, before there is a session
	flashCookie   = "dashboard_flash"
)

// dashboardTemplates holds one template set per page. Each page defines "title" and "content",
// and the shared layout calls them; parsed into a single set, each page's "content" would
// overwrite the previous one.
var dashboardTemplates = func() map[string]*template.Template {
	funcs := template.FuncMap{
		"money": formatMoney,
		"major": formatMajor,
		"date":  func(t time.Time) string { return t.Local().Format("Jan 2, 15:04") },
		"badge": badgeClass,
	}
	layout := template.Must(template.New("").Funcs(funcs).ParseFS(webFS, "web/templates/dashboard/layout.html"))
	pages, err := fs.Glob(webFS, "web/templates/dashboard/*.html")
	if err != nil {
		panic(err)
	}
	sets := map[string]*template.Template{}
	for _, p := range pages {
		name := path.Base(p)
		if name == "layout.html" {
			continue
		}
		sets[name] = template.Must(template.Must(layout.Clone()).ParseFS(webFS, p))
	}
	return sets
}()

// dashPage is what every dashboard template gets.
type dashPage struct {
	Title   string
	Nav     string // which top-level section is active
	Session *dashboard.Session
	Flash   string
	Error   string
	Data    any
}

func renderDashboard(w http.ResponseWriter, r *http.Request, status int, name string, page dashPage) {
	if page.Flash == "" {
		page.Flash = takeFlash(w, r)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := dashboardTemplates[name].ExecuteTemplate(w, "layout", page); err != nil {
		loggerFrom(r.Context()).Error("render dashboard", "template", name, "error", err)
	}
}

// withDashboardHeaders is withPageHeaders plus the one script the dashboard loads, htmx, served
// from this server.
func withDashboardHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; "+
			"connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

type dashSessionKey struct{}

func sessionFrom(ctx context.Context) dashboard.Session {
	s, _ := ctx.Value(dashSessionKey{}).(dashboard.Session)
	return s
}

// requireSession lets only signed-in browsers through, and every POST must carry the session's
// CSRF token: a form on another site can make the browser send the cookie, but cannot read the token.
func requireSession(auth dashboardAuth, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			http.Redirect(w, r, "/dashboard/login", http.StatusSeeOther)
			return
		}
		sess, err := auth.Session(r.Context(), c.Value)
		if errors.Is(err, dashboard.ErrNoSession) {
			http.Redirect(w, r, "/dashboard/login", http.StatusSeeOther)
			return
		}
		if err != nil {
			dashboardInternalError(w, r, "dashboard session", err)
			return
		}
		if r.Method == http.MethodPost {
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
			if err := r.ParseForm(); err != nil || !tokensEqual(r.PostForm.Get("csrf_token"), sess.CSRFToken) {
				renderDashboard(w, r, http.StatusForbidden, "error.html", dashPage{Title: "Forbidden", Session: &sess,
					Error: "This form has expired. Go back, reload the page and try again."})
				return
			}
		}
		next(w, r.WithContext(context.WithValue(r.Context(), dashSessionKey{}, sess)))
	})
}

func tokensEqual(got, want string) bool {
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func dashboardInternalError(w http.ResponseWriter, r *http.Request, msg string, err error) {
	loggerFrom(r.Context()).Error(msg, "error", err)
	var sess *dashboard.Session
	if s := sessionFrom(r.Context()); s.MerchantID != 0 {
		sess = &s
	}
	renderDashboard(w, r, http.StatusInternalServerError, "error.html", dashPage{Title: "Error", Session: sess,
		Error: "Something went wrong on our end. Nothing was changed by this page; try again."})
}

// cookies sets and clears the dashboard's cookies. secure is on whenever the dashboard is served
// over HTTPS, so the session token never travels in clear text.
type cookies struct{ secure bool }

func (c cookies) set(w http.ResponseWriter, name, value, path string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: path, MaxAge: int(maxAge.Seconds()),
		HttpOnly: true, Secure: c.secure, SameSite: http.SameSiteLaxMode})
}

func (c cookies) clear(w http.ResponseWriter, name, path string) {
	http.SetCookie(w, &http.Cookie{Name: name, Path: path, MaxAge: -1, HttpOnly: true, Secure: c.secure, SameSite: http.SameSiteLaxMode})
}

var flashMessages = map[string]string{
	"captured":       "Payment captured.",
	"canceled":       "Payment canceled. Any held money was released.",
	"refunded":       "Refund sent.",
	"refund_pending": "Refund sent; the bank has not answered yet. It stays pending until it does.",
	"refund_failed":  "The card issuer refused the refund. No money moved.",
	"key_revoked":    "API key revoked. Requests with it now fail.",
	"mode_test":      "Viewing test data.",
	"mode_live":      "Viewing live data.",
}

// redirectWithFlash answers a form POST with 303 to a GET page that shows a one-time message, so
// reloading the result never repeats the action. Only known message codes are stored.
func redirectWithFlash(w http.ResponseWriter, r *http.Request, c cookies, to, flash string) {
	c.set(w, flashCookie, flash, "/dashboard", time.Minute)
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func takeFlash(w http.ResponseWriter, r *http.Request) string {
	c, err := r.Cookie(flashCookie)
	if err != nil {
		return ""
	}
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Path: "/dashboard", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	return flashMessages[c.Value]
}

type loginForm struct {
	Email string
	Token string
}

func handleLoginPage(auth dashboardAuth, c cookies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if sc, err := r.Cookie(sessionCookie); err == nil {
			if _, err := auth.Session(r.Context(), sc.Value); err == nil {
				http.Redirect(w, r, "/dashboard/", http.StatusSeeOther)
				return
			}
		}
		renderLogin(w, r, c, http.StatusOK, "", "")
	}
}

func renderLogin(w http.ResponseWriter, r *http.Request, c cookies, status int, email, msg string) {
	token := dashboard.NewToken()
	c.set(w, loginCookie, token, "/dashboard/login", time.Hour)
	renderDashboard(w, r, status, "login.html", dashPage{Title: "Sign in", Error: msg, Data: loginForm{Email: email, Token: token}})
}

func handleLogin(auth dashboardAuth, c cookies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		if err := r.ParseForm(); err != nil {
			renderLogin(w, r, c, http.StatusBadRequest, "", "The form could not be read. Try again.")
			return
		}
		email := strings.TrimSpace(r.PostForm.Get("email"))
		lc, err := r.Cookie(loginCookie)
		if err != nil || !tokensEqual(r.PostForm.Get("csrf_token"), lc.Value) {
			renderLogin(w, r, c, http.StatusForbidden, email, "This form has expired. Try again.")
			return
		}
		merchantID, err := auth.Login(r.Context(), email, r.PostForm.Get("password"))
		switch {
		case errors.Is(err, dashboard.ErrBadCredentials):
			renderLogin(w, r, c, http.StatusUnauthorized, email, "Wrong email or password.")
			return
		case errors.Is(err, dashboard.ErrTooManyLogins):
			renderLogin(w, r, c, http.StatusTooManyRequests, email, "Too many failed sign-ins. Try again in 15 minutes.")
			return
		case err != nil:
			dashboardInternalError(w, r, "dashboard login", err)
			return
		}
		token, err := auth.CreateSession(r.Context(), merchantID)
		if err != nil {
			dashboardInternalError(w, r, "dashboard login", err)
			return
		}
		c.clear(w, loginCookie, "/dashboard/login")
		c.set(w, sessionCookie, token, "/dashboard", dashboard.SessionLifetime)
		http.Redirect(w, r, "/dashboard/", http.StatusSeeOther)
	}
}

func handleLogout(auth dashboardAuth, c cookies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := auth.DeleteSession(r.Context(), sessionFrom(r.Context())); err != nil {
			dashboardInternalError(w, r, "dashboard logout", err)
			return
		}
		c.clear(w, sessionCookie, "/dashboard")
		http.Redirect(w, r, "/dashboard/login", http.StatusSeeOther)
	}
}

// handleSwitchMode flips between test and live data. It lands on the home page, since the page the
// merchant was on belongs to the other mode and would not be found.
func handleSwitchMode(auth dashboardAuth, c cookies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		live := r.PostForm.Get("livemode") == "true"
		if err := auth.SetLivemode(r.Context(), sessionFrom(r.Context()), live); err != nil {
			dashboardInternalError(w, r, "switch mode", err)
			return
		}
		flash := "mode_test"
		if live {
			flash = "mode_live"
		}
		redirectWithFlash(w, r, c, "/dashboard/", flash)
	}
}
