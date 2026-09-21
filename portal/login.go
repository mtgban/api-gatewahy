package portal

import (
	"errors"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/api-gatewahy/session"
)

const (
	magicLinkTTL = 15 * time.Minute
	pendingTTL   = time.Hour
	tryAgainMsg  = "Something went wrong on our side. Try again in a minute."
)

// loginData is login.html's payload.
type loginData struct {
	PlanText   string
	PatreonURL string
}

type sentData struct {
	Email   string
	Minutes int
}

// loginConfirmData is login_confirm.html's payload.
type loginConfirmData struct {
	Token string
}

func (s *Server) registerLogin(mux *http.ServeMux) {
	mux.HandleFunc("GET /login", s.loginForm)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("GET /login/{token}", s.loginConfirm)
	mux.HandleFunc("POST /login/{token}", s.loginToken)
	mux.HandleFunc("POST /logout", s.logout)
}

func (s *Server) loginLinksPerHour() int {
	if s.LoginLinksPerHour > 0 {
		return s.LoginLinksPerHour
	}
	return 5
}

// loginForm shows the email form; a signed-in reader goes to the account page.
func (s *Server) loginForm(w http.ResponseWriter, r *http.Request) {
	if _, _, err := s.current(r); err == nil {
		http.Redirect(w, r, "/account", http.StatusFound)
		return
	}
	pv, _ := s.Sessions.Pending(r)
	s.renderLogin(w, r, http.StatusOK, "", pv)
}

// renderLogin draws the form, describing the pending plan when there is one.
func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, status int, errMsg string, pending url.Values) {
	returnTo := s.PricingURL
	d := loginData{}
	if pending != nil {
		returnTo = validReturnTo(pending.Get("return_to"), s.PricingURL)
		if pending.Get("package") != "" {
			if plan, err := planFromValues(pending).Validate(s.Catalog, s.Games, pending.Get("invite") != ""); err == nil {
				d.PlanText = plan.Describe(s.Catalog)
			}
		}
	}
	d.PatreonURL = siteOrigin(returnTo) + "/api-login"
	p := s.pageFor(nil, "Sign in")
	p.Error = errMsg
	p.Data = d
	s.render(w, status, "login.html", p)
}

// login sends a magic link. The page is the same whether the account existed.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	pv, _ := s.Sessions.Pending(r)
	if !s.sameOrigin(r) {
		s.renderLogin(w, r, http.StatusForbidden, "That request did not come from this site. Use the form on this page.", pv)
		return
	}
	email := apiaccess.NormalizeEmail(r.FormValue("email"))
	if addr, err := mail.ParseAddress(email); len(email) > 254 || err != nil || addr.Address != email || strings.ContainsAny(email, " <>") {
		s.renderLogin(w, r, http.StatusBadRequest, "Enter your email address.", pv)
		return
	}
	now := s.now()
	if !s.limit.allow("ip:"+s.clientIP(r), s.loginLinksPerHour(), now) || !s.limit.allow("email:"+email, s.loginLinksPerHour(), now) {
		s.renderLogin(w, r, http.StatusTooManyRequests, "Too many sign-in links requested. Try again in an hour.", pv)
		return
	}
	ctx := r.Context()
	a, err := s.Store.GetOrCreateAccount(ctx, email, "self-service")
	if err != nil {
		s.logf("login %s: account: %v", email, err)
		s.renderLogin(w, r, http.StatusInternalServerError, tryAgainMsg, pv)
		return
	}
	token, err := s.Store.CreateMagicLink(ctx, a.ID, magicLinkTTL)
	if err != nil {
		s.logf("login %s: link: %v", email, err)
		s.renderLogin(w, r, http.StatusInternalServerError, tryAgainMsg, pv)
		return
	}
	subject, text, htmlBody := magicLinkMail(s.PublicURL+"/login/"+url.PathEscape(token), magicLinkTTL)
	if err := s.Mail.Send(ctx, a.Email, subject, text, htmlBody); err != nil {
		s.logf("login %s: mail: %v", email, err)
		_ = s.Store.DeleteMagicLink(ctx, token)
		s.renderLogin(w, r, http.StatusBadGateway, "We could not send the email. Try again in a minute.", pv)
		return
	}
	p := s.pageFor(nil, "Check your inbox")
	p.Data = sentData{Email: a.Email, Minutes: int(magicLinkTTL.Minutes())}
	s.render(w, http.StatusOK, "sent.html", p)
}

// loginConfirm shows a confirm button; mail scanners prefetch links, so the GET must not consume.
func (s *Server) loginConfirm(w http.ResponseWriter, r *http.Request) {
	p := s.pageFor(nil, "Finish signing in")
	p.Data = loginConfirmData{Token: r.PathValue("token")}
	s.render(w, http.StatusOK, "login_confirm.html", p)
}

// loginToken consumes a magic link and starts the session.
func (s *Server) loginToken(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		s.fail(w, r, http.StatusForbidden, "That request did not come from this site. Open the link again and use the button on the page.")
		return
	}
	a, err := s.Store.ConsumeMagicLink(r.Context(), r.PathValue("token"), s.now())
	if errors.Is(err, apiaccess.ErrNotFound) {
		pv, _ := s.Sessions.Pending(r)
		s.renderLogin(w, r, http.StatusBadRequest, "That sign-in link is invalid or has expired. Request a new one.", pv)
		return
	}
	if err != nil {
		s.logf("login token: %v", err)
		s.fail(w, r, http.StatusInternalServerError, tryAgainMsg)
		return
	}
	if a.Status != "active" {
		s.fail(w, r, http.StatusForbidden, suspendedMsg)
		return
	}
	s.Sessions.Issue(w, session.Session{AccountID: a.ID, Email: a.Email})
	s.afterLogin(w, r)
}

// afterLogin continues a pending checkout or lands on the account page.
func (s *Server) afterLogin(w http.ResponseWriter, r *http.Request) {
	if pv, err := s.Sessions.Pending(r); err == nil && pv.Get("package") != "" {
		http.Redirect(w, r, "/checkout", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/account?notice=login", http.StatusFound)
}

// logout clears cookies for any signed-in reader, even a suspended one, as long as the CSRF token checks out.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if sess, err := s.Sessions.Read(r); err == nil && !s.Sessions.CheckCSRF(sess, r.FormValue("csrf")) {
		http.Error(w, csrfExpiredMsg, http.StatusForbidden)
		return
	}
	s.Sessions.Clear(w)
	s.Sessions.ClearPending(w)
	http.Redirect(w, r, s.PricingURL, http.StatusFound)
}
