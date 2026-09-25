// Package portal is the gateway's customer and admin web: login by magic
// link, checkout confirmation, the account page, the Patreon trial and
// sign-in handoff, and admin. Pages are embedded templates; every state
// change is a POST carrying the session's CSRF token.
package portal

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/api-gatewahy/billing"
	"github.com/mtgban/api-gatewahy/gateway"
	"github.com/mtgban/api-gatewahy/mailer"
	"github.com/mtgban/api-gatewahy/session"
	"github.com/mtgban/mtgban-website/apiproductlist"
)

//go:embed templates/*.html static/*.css
var assets embed.FS

// Store is the slice of apiaccess.Client the portal reads and writes.
type Store interface {
	GetAccount(ctx context.Context, id int64) (apiaccess.Account, error)
	GetAccountByEmail(ctx context.Context, email string) (apiaccess.Account, error)
	GetOrCreateAccount(ctx context.Context, email, note string) (apiaccess.Account, error)
	SetAccountStatus(ctx context.Context, id int64, status string) error
	SetAccountNote(ctx context.Context, id int64, note string) error
	ListAccounts(ctx context.Context) ([]apiaccess.Account, error)
	SearchAccounts(ctx context.Context, q string) ([]apiaccess.Account, error)
	CreateMagicLink(ctx context.Context, accountID int64, ttl time.Duration) (string, error)
	ConsumeMagicLink(ctx context.Context, token string, now time.Time) (apiaccess.Account, error)
	DeleteMagicLink(ctx context.Context, token string) error
	CreateKey(ctx context.Context, accountID int64, label string, kind apiaccess.KeyKind) (string, apiaccess.Key, error)
	ListKeys(ctx context.Context, accountID int64) ([]apiaccess.Key, error)
	RevokeKey(ctx context.Context, id, accountID int64) (apiaccess.Key, error)
	ListEntitlements(ctx context.Context, accountID int64) ([]apiaccess.Entitlement, error)
	AddEntitlement(ctx context.Context, e apiaccess.Entitlement) (apiaccess.Entitlement, error)
	EndEntitlement(ctx context.Context, id int64, at time.Time) error
	SummarizeUsage(ctx context.Context, since, until time.Time, accountID int64) ([]apiaccess.UsageRow, error)
	CreateTrial(ctx context.Context, email string, accountID int64, endsAt, notBefore time.Time) (apiaccess.Trial, error)
	DeleteTrial(ctx context.Context, id int64) error
	LastTrial(ctx context.Context, email string) (apiaccess.Trial, error)
	TrialsToRemind(ctx context.Context, from, to time.Time) ([]apiaccess.Trial, error)
	MarkTrialReminded(ctx context.Context, id int64, at time.Time) error
	CreateInvite(ctx context.Context, intervalKey, email string, ttl time.Duration, note string) (string, apiaccess.Invite, error)
	Notify(ctx context.Context, payload string) error
	BumpSessionEpoch(ctx context.Context, accountID int64) (int64, error)
	ConsumeNonce(ctx context.Context, nonce string, expiresAt, now time.Time) error
	RecordAdminAction(ctx context.Context, actor, action string, accountID int64, target, detail string) error
	ListAdminActions(ctx context.Context, accountID int64, limit int) ([]apiaccess.AdminAction, error)
}

var _ Store = (*apiaccess.Client)(nil)

// reserved are the portal's fixed routes; the Stripe landing paths must not collide with them.
var reservedExact = []string{"/", "/login", "/logout", "/account", "/portal", "/trial", "/session", "/admin", "/checkout", "/static/portal.css", "/healthz", "/stripe/webhook"}
var reservedPrefixes = []string{"/login/", "/account/", "/admin/", "/static/", "/v1/"}

// Reserved reports whether path is a portal route or under one.
func Reserved(path string) bool {
	if slices.Contains(reservedExact, path) {
		return true
	}
	for _, p := range reservedPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// Server serves the customer and admin pages.
type Server struct {
	Store       Store
	Catalog     *apiproductlist.ProductList
	Games       []string
	KnownStores []string
	Sessions    *session.Codec
	Mail        mailer.Mailer
	// Stripe, Checkout, Reconcile, and ReconcileAll are nil when billing is off
	Stripe       billing.API
	Checkout     *billing.Checkout
	Reconcile    func(ctx context.Context, subID string) error
	ReconcileAll func(ctx context.Context) (billing.Result, error)
	PublicURL    string
	PricingURL   string
	SuccessPath  string
	CancelPath   string
	AdminEmails  []string
	TrialDays    int
	// GameSecrets are the per-game secrets the gateway calls the sites with; a
	// handoff token is verified with the secret of the game that minted it.
	GameSecrets       map[string][]byte
	LoginLinksPerHour int
	ClientIPHeader    string
	Now               func() time.Time
	Log               *log.Logger

	once  sync.Once
	tmpl  *template.Template
	css   []byte
	limit limiter
}

// page is what every template receives.
type page struct {
	Title      string
	Session    *session.Session
	CSRF       string
	Error      string
	Notice     string
	PricingURL string
	PrivacyURL string
	PublicURL  string
	// Wide lets a page with several tables use more of the viewport.
	Wide bool
	Data any
}

// errorData drives error.html's back link.
type errorData struct {
	BackURL  string
	BackText string
}

var (
	errNoSession = errors.New("portal: no session")
	errSuspended = errors.New("portal: account suspended")
)

const suspendedMsg = "This account is suspended. Contact administrator@mtgban.com if you think that is a mistake."
const csrfExpiredMsg = "this form expired, go back and try again"

var funcs = template.FuncMap{
	"usd": billing.Dollars,
	"date": func(v any) string {
		switch t := v.(type) {
		case time.Time:
			return t.Format("2006-01-02")
		case *time.Time:
			if t == nil {
				return "-"
			}
			return t.Format("2006-01-02")
		}
		return "-"
	},
	"join": strings.Join,
	"datetime": func(t time.Time) string {
		return t.UTC().Format("2006-01-02 15:04")
	},
	"host": func(u string) string {
		p, err := url.Parse(u)
		if err != nil {
			return ""
		}
		return p.Host
	},
}

func (s *Server) init() {
	s.once.Do(func() {
		s.tmpl = template.Must(template.New("").Funcs(funcs).ParseFS(assets, "templates/*.html"))
		css, err := assets.ReadFile("static/portal.css")
		if err != nil {
			panic(err)
		}
		s.css = css
	})
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log.Printf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// Register mounts every portal route on mux.
func (s *Server) Register(mux *http.ServeMux) {
	s.init()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, s.PricingURL, http.StatusFound)
	})
	mux.HandleFunc("GET /static/portal.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write(s.css)
	})
	s.registerLogin(mux)
	s.registerCheckout(mux)
	s.registerAccount(mux)
	s.registerTrial(mux)
	s.registerAdmin(mux)
}

// pageFor is the common page frame for sess.
func (s *Server) pageFor(sess *session.Session, title string) page {
	p := page{Title: title, Session: sess, PricingURL: s.PricingURL, PublicURL: s.PublicURL, PrivacyURL: siteOrigin(s.PricingURL) + "/privacy"}
	if sess != nil {
		p.CSRF = s.Sessions.CSRF(*sess)
	}
	return p
}

// render executes the named page. Templates render to a buffer first so an
// execution error becomes a 500 rather than half a page.
func (s *Server) render(w http.ResponseWriter, status int, name string, p page) {
	s.init()
	if p.PricingURL == "" {
		p.PricingURL = s.PricingURL
	}
	if p.PrivacyURL == "" {
		p.PrivacyURL = siteOrigin(s.PricingURL) + "/privacy"
	}
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, p); err != nil {
		s.logf("render %s: %v", name, err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// fail renders the error page with msg and a link back to the pricing page.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, status int, msg string) {
	var sess *session.Session
	if got, err := s.Sessions.Read(r); err == nil {
		sess = &got
	}
	p := s.pageFor(sess, "Something went wrong")
	p.Error = msg
	p.Data = errorData{BackURL: s.PricingURL, BackText: "Back to the API page"}
	s.render(w, status, "error.html", p)
}

// current returns the signed-in account, errNoSession, or errSuspended.
func (s *Server) current(r *http.Request) (session.Session, apiaccess.Account, error) {
	sess, err := s.Sessions.Read(r)
	if err != nil {
		return session.Session{}, apiaccess.Account{}, errNoSession
	}
	a, err := s.Store.GetAccount(r.Context(), sess.AccountID)
	if err != nil {
		if !errors.Is(err, apiaccess.ErrNotFound) {
			s.logf("session account %d: %v", sess.AccountID, err)
		}
		return session.Session{}, apiaccess.Account{}, errNoSession
	}
	// A logout bumps the epoch, which signs out every cookie issued before it.
	if sess.Epoch != a.SessionEpoch {
		return session.Session{}, apiaccess.Account{}, errNoSession
	}
	if a.Status != "active" {
		return sess, a, errSuspended
	}
	return sess, a, nil
}

// withSession runs h for a signed-in active account and checks CSRF on POST.
func (s *Server) withSession(h func(w http.ResponseWriter, r *http.Request, sess session.Session, a apiaccess.Account)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, a, err := s.current(r)
		switch {
		case errors.Is(err, errSuspended):
			s.fail(w, r, http.StatusForbidden, suspendedMsg)
			return
		case err != nil && r.Method == http.MethodGet:
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		case err != nil:
			http.Error(w, "sign in first", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost && !s.Sessions.CheckCSRF(sess, r.FormValue("csrf")) {
			http.Error(w, csrfExpiredMsg, http.StatusForbidden)
			return
		}
		h(w, r, sess, a)
	}
}

// isAdmin reports whether email is in admin_emails.
func (s *Server) isAdmin(email string) bool {
	for _, e := range s.AdminEmails {
		if apiaccess.NormalizeEmail(e) == apiaccess.NormalizeEmail(email) {
			return true
		}
	}
	return false
}

// withAdmin is withSession plus the admin_emails check; others get 404.
func (s *Server) withAdmin(h func(w http.ResponseWriter, r *http.Request, sess session.Session, a apiaccess.Account)) http.HandlerFunc {
	return s.withSession(func(w http.ResponseWriter, r *http.Request, sess session.Session, a apiaccess.Account) {
		if !s.isAdmin(a.Email) {
			http.NotFound(w, r)
			return
		}
		h(w, r, sess, a)
	})
}

// sameOrigin reports whether a state-changing request came from a page on this host.
// Browsers send Sec-Fetch-Site on form posts; Origin is the fallback for the ones that do not.
func (s *Server) sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin":
		return true
	case "cross-site", "same-site":
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		// Referer is only present on posts from outside the portal, which no-referrer strips on our own pages.
		origin = siteOrigin(r.Header.Get("Referer"))
	}
	return origin != "" && strings.EqualFold(origin, siteOrigin(s.PublicURL))
}

// validReturnTo accepts an https URL on mtgban.com or a subdomain, or
// http://localhost for local runs; anything else becomes fallback.
func validReturnTo(raw, fallback string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Host == "" {
		return fallback
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case u.Scheme == "https" && (host == "mtgban.com" || strings.HasSuffix(host, ".mtgban.com")):
		return u.String()
	case u.Scheme == "http" && host == "localhost":
		return u.String()
	}
	return fallback
}

// siteOrigin is scheme://host of u, or "" when u does not parse.
func siteOrigin(u string) string {
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return ""
	}
	return p.Scheme + "://" + p.Host
}

// clientIP trusts the configured header when it holds an IP, else the peer.
func (s *Server) clientIP(r *http.Request) string {
	return gateway.ClientIP(r, s.ClientIPHeader)
}

// notices are the messages a redirect may ask the next page to show.
var notices = map[string]string{
	"revoked": "Key revoked.",
	"plan":    "Plan changed. Your access updates within a minute.",
	"trial":   "Your trial has started. Create a key below to begin.",
	"login":   "You are signed in.",
	"status":  "Status updated.",
	"saved":   "Note saved.",
	"granted": "Entitlement added.",
	"ended":   "Entitlement ended.",
}

func noticeFor(r *http.Request) string {
	return notices[r.URL.Query().Get("notice")]
}
