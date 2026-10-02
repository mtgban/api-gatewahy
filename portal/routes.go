package portal

import (
	"net/http"
	"slices"
	"strings"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/api-gatewahy/session"
)

// guard is how Register protects a route before calling its handler.
type guard int

const (
	guardPublic guard = iota
	guardSameOrigin
	guardSession
	guardAdmin
	// guardCSRFIfSignedIn checks CSRF only when a session cookie is present;
	// a signed-out request is not an error. Always handled inline: logout.
	guardCSRFIfSignedIn
)

// sessionHandlerFunc is the signature withSession and withAdmin hand a signed-in account to.
type sessionHandlerFunc func(w http.ResponseWriter, r *http.Request, sess session.Session, a apiaccess.Account)

// route is one portal endpoint. plain handles guardPublic, guardSameOrigin,
// and guardCSRFIfSignedIn; session handles guardSession and guardAdmin.
type route struct {
	method  string
	pattern string
	guard   guard
	plain   http.HandlerFunc
	session sessionHandlerFunc
	// inline is legal only on guardSameOrigin: the handler already performs
	// that check itself, with its own distinct failure response (/login).
	inline bool
}

// home redirects the bare domain to the pricing page.
func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, s.PricingURL, http.StatusFound)
}

// serveCSS writes the portal's one stylesheet.
func (s *Server) serveCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(s.css)
}

// fixedRoutes is every portal route whose pattern does not depend on config.
// Safe on a zero-value *Server: handlers are referenced as values, never invoked.
func fixedRoutes(s *Server) []route {
	return []route{
		{method: "GET", pattern: "/{$}", guard: guardPublic, plain: s.home},
		{method: "GET", pattern: "/static/portal.css", guard: guardPublic, plain: s.serveCSS},

		{method: "GET", pattern: "/login", guard: guardPublic, plain: s.loginForm},
		// login keeps its own same-origin check inline: it re-renders the
		// login form with the pending plan, not the generic failure page.
		{method: "POST", pattern: "/login", guard: guardSameOrigin, plain: s.login, inline: true},
		{method: "GET", pattern: "/login/{token}", guard: guardPublic, plain: s.loginConfirm},
		{method: "POST", pattern: "/login/{token}", guard: guardSameOrigin, plain: s.loginToken},
		{method: "POST", pattern: "/logout", guard: guardCSRFIfSignedIn, plain: s.logout},

		{method: "GET", pattern: "/checkout", guard: guardPublic, plain: s.checkoutGet},
		{method: "POST", pattern: "/checkout", guard: guardSession, session: s.checkoutPost},

		{method: "GET", pattern: "/account", guard: guardSession, session: s.account},
		{method: "POST", pattern: "/account/keys", guard: guardSession, session: s.createKey},
		{method: "POST", pattern: "/account/keys/{id}/revoke", guard: guardSession, session: s.revokeKey},
		{method: "POST", pattern: "/account/plan", guard: guardSession, session: s.changePlan},
		{method: "GET", pattern: "/portal", guard: guardSession, session: s.portal},

		{method: "GET", pattern: "/trial", guard: guardPublic, plain: s.trialConfirm},
		{method: "POST", pattern: "/trial", guard: guardSameOrigin, plain: s.trial},
		{method: "GET", pattern: "/session", guard: guardPublic, plain: s.sessionConfirm},
		{method: "POST", pattern: "/session", guard: guardSameOrigin, plain: s.patreonSession},

		{method: "GET", pattern: "/admin", guard: guardAdmin, session: s.adminHome},
		{method: "POST", pattern: "/admin/reconcile", guard: guardAdmin, session: s.adminReconcile},
		{method: "GET", pattern: "/admin/usage", guard: guardAdmin, session: s.adminUsage},
		{method: "GET", pattern: "/admin/accounts/{id}", guard: guardAdmin, session: s.adminAccount},
		{method: "POST", pattern: "/admin/accounts/{id}/status", guard: guardAdmin, session: s.adminStatus},
		{method: "POST", pattern: "/admin/accounts/{id}/note", guard: guardAdmin, session: s.adminNote},
		{method: "POST", pattern: "/admin/accounts/{id}/keys/{kid}/revoke", guard: guardAdmin, session: s.adminRevokeKey},
		{method: "POST", pattern: "/admin/accounts/{id}/entitlements", guard: guardAdmin, session: s.adminAddEntitlement},
		{method: "POST", pattern: "/admin/accounts/{id}/entitlements/{eid}/end", guard: guardAdmin, session: s.adminEndEntitlement},
		{method: "POST", pattern: "/admin/accounts/{id}/invites", guard: guardAdmin, session: s.adminInvite},
	}
}

// routes is every route Register mounts: the fixed table plus the Stripe
// landing paths, which come from config rather than a literal pattern.
func (s *Server) routes() []route {
	rs := fixedRoutes(s)
	rs = append(rs,
		route{method: "GET", pattern: s.SuccessPath, guard: guardSession, session: s.success},
		route{method: "GET", pattern: s.CancelPath, guard: guardPublic, plain: s.cancel},
	)
	return rs
}

// Register mounts every portal route on mux.
func (s *Server) Register(mux *http.ServeMux) {
	s.init()
	for _, rt := range s.routes() {
		mux.HandleFunc(rt.method+" "+rt.pattern, s.wrap(rt))
	}
}

// wrap applies rt's guard, unless the handler already checks it inline.
func (s *Server) wrap(rt route) http.HandlerFunc {
	switch rt.guard {
	case guardSameOrigin:
		if rt.inline {
			return rt.plain
		}
		return s.requireSameOrigin(rt.plain)
	case guardSession:
		return s.withSession(rt.session)
	case guardAdmin:
		return s.withAdmin(rt.session)
	default:
		// guardPublic and guardCSRFIfSignedIn both run the handler directly.
		return rt.plain
	}
}

// nonPortalReservedExact and nonPortalReservedPrefixes are fixed outside the
// portal's table: health checks, the Stripe webhook, static assets, and the API.
var nonPortalReservedExact = []string{"/healthz", "/stripe/webhook"}
var nonPortalReservedPrefixes = []string{"/static/", "/v1/"}

// Reserved reports whether path is a portal route or under one; SuccessPath
// and CancelPath are excluded, since those are checked against this.
func Reserved(path string) bool {
	exact, prefixes := reservedFromRoutes()
	for _, p := range exact {
		if path == p {
			return true
		}
	}
	for _, p := range prefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// reservedFromRoutes splits the fixed table's patterns into literal paths
// and, for every pattern with more than one segment, its first-segment prefix.
func reservedFromRoutes() (exact, prefixes []string) {
	exact = append(exact, nonPortalReservedExact...)
	prefixes = append(prefixes, nonPortalReservedPrefixes...)
	for _, rt := range fixedRoutes(&Server{}) {
		p := rt.pattern
		if p == "/{$}" {
			p = "/"
		}
		if !strings.Contains(p, "{") && !slices.Contains(exact, p) {
			exact = append(exact, p)
		}
		if j := strings.IndexByte(p[1:], '/'); j >= 0 {
			if prefix := p[:j+2]; !slices.Contains(prefixes, prefix) {
				prefixes = append(prefixes, prefix)
			}
		}
	}
	return exact, prefixes
}
