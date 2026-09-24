package portal

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/api-gatewahy/session"
	"github.com/mtgban/mtgban-website/apihandoff"
)

// trialCooldown is how long after one trial starts the same Patreon email may start another.
const trialCooldown = 180 * 24 * time.Hour

type trialDeniedData struct {
	Next string
}

// trialConfirmData is trial_confirm.html's payload.
type trialConfirmData struct {
	Email    string
	Name     string
	Token    string
	Days     int
	ReturnTo string
}

// sessionConfirmData is session_confirm.html's payload.
type sessionConfirmData struct {
	Email    string
	Name     string
	Token    string
	ReturnTo string
}

func (s *Server) registerTrial(mux *http.ServeMux) {
	mux.HandleFunc("GET /trial", s.trialConfirm)
	mux.HandleFunc("POST /trial", s.trial)
	mux.HandleFunc("GET /session", s.sessionConfirm)
	mux.HandleFunc("POST /session", s.patreonSession)
}

// verifyHandoff checks the token's signature, expiry, and purpose without
// touching its nonce. The game named in the token picks the secret; a token
// for a game this gateway does not serve has no secret to check against.
func (s *Server) verifyHandoff(token, purpose string) (apihandoff.Claims, bool) {
	secret, ok := s.GameSecrets[apihandoff.Game(token)]
	if !ok || len(secret) == 0 {
		return apihandoff.Claims{}, false
	}
	c, err := apihandoff.Verify(secret, token, s.now())
	if err != nil || c.Purpose != purpose {
		return apihandoff.Claims{}, false
	}
	return c, true
}

// handoffClaims verifies the token for one purpose and burns its nonce.
// r.FormValue reads the query string on GET and the body on POST.
func (s *Server) handoffClaims(r *http.Request, purpose string) (apihandoff.Claims, bool) {
	c, ok := s.verifyHandoff(r.FormValue("t"), purpose)
	if !ok {
		return apihandoff.Claims{}, false
	}
	if err := s.Store.ConsumeNonce(r.Context(), c.Nonce, c.Expires, s.now()); err != nil {
		if !errors.Is(err, apiaccess.ErrNonceUsed) {
			s.logf("handoff nonce: %v", err)
		}
		return apihandoff.Claims{}, false
	}
	return c, true
}

// trialDays is the configured trial length in days, defaulting to 15.
func (s *Server) trialDays() int {
	if s.TrialDays > 0 {
		return s.TrialDays
	}
	return 15
}

// handoffFailed is the one page every bad token gets, so nothing leaks about why.
func (s *Server) handoffFailed(w http.ResponseWriter, title string) {
	p := s.pageFor(nil, title)
	p.Data = errorData{BackURL: s.PricingURL, BackText: "Back to the API page"}
	s.render(w, http.StatusBadRequest, "trial_error.html", p)
}

// trialConfirm shows the confirm page for a trial token without spending its nonce.
func (s *Server) trialConfirm(w http.ResponseWriter, r *http.Request) {
	token := r.FormValue("t")
	c, ok := s.verifyHandoff(token, apihandoff.PurposeTrial)
	if !ok {
		s.handoffFailed(w, "Trial link expired")
		return
	}
	returnTo := validReturnTo(r.FormValue("return_to"), s.PricingURL)
	p := s.pageFor(nil, "Start your API trial")
	p.Data = trialConfirmData{Email: apiaccess.NormalizeEmail(c.Email), Name: c.Name, Token: token, Days: s.trialDays(), ReturnTo: returnTo}
	s.render(w, http.StatusOK, "trial_confirm.html", p)
}

// trial grants a 15-day all-access trial to a Patreon supporter and signs them in.
func (s *Server) trial(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		s.fail(w, r, http.StatusForbidden, "That request did not come from this site. Open the link again and use the button on the page.")
		return
	}
	claims, ok := s.handoffClaims(r, apihandoff.PurposeTrial)
	if !ok {
		s.handoffFailed(w, "Trial link expired")
		return
	}
	returnTo := validReturnTo(r.FormValue("return_to"), s.PricingURL)
	ctx := r.Context()
	now := s.now()
	a, err := s.Store.GetOrCreateAccount(ctx, claims.Email, "patreon trial")
	if err != nil {
		s.logf("trial %s: account: %v", claims.Email, err)
		s.fail(w, r, http.StatusInternalServerError, tryAgainMsg)
		return
	}
	if a.Status != "active" {
		s.fail(w, r, http.StatusForbidden, suspendedMsg)
		return
	}
	t, err := s.Store.CreateTrial(ctx, claims.Email, a.ID, now.AddDate(0, 0, s.trialDays()), now.Add(-trialCooldown))
	if errors.Is(err, apiaccess.ErrTrialTooSoon) {
		next := "later"
		if last, err := s.Store.LastTrial(ctx, claims.Email); err == nil {
			next = last.GrantedAt.Add(trialCooldown).Format("January 2, 2006")
		}
		p := s.pageFor(nil, "Trial already used")
		p.Data = trialDeniedData{Next: next}
		s.render(w, http.StatusOK, "trial_denied.html", p)
		return
	}
	if err != nil {
		s.logf("trial %s: %v", claims.Email, err)
		s.fail(w, r, http.StatusInternalServerError, tryAgainMsg)
		return
	}
	until := t.EndsAt
	e := apiaccess.Entitlement{AccountID: a.ID, Source: "trial", Games: slices.Clone(s.Games), StoreScope: apiaccess.ScopeAll,
		Modes: slices.Clone(apiaccess.ValidModes), ValidUntil: &until, Note: "patreon trial"}
	if _, err := s.Store.AddEntitlement(ctx, e); err != nil {
		s.logf("trial %s: entitlement: %v", claims.Email, err)
		if delErr := s.Store.DeleteTrial(ctx, t.ID); delErr != nil {
			s.logf("trial %s: rollback: %v", claims.Email, delErr)
		}
		s.fail(w, r, http.StatusInternalServerError, tryAgainMsg)
		return
	}
	if err := s.Store.Notify(ctx, ""); err != nil {
		s.logf("trial notify: %v", err)
	}
	subject, text, htmlBody := trialStartedMail(until, s.PublicURL+"/account", s.trialDays())
	if err := s.Mail.Send(ctx, a.Email, subject, text, htmlBody); err != nil {
		s.logf("trial mail %s: %v", a.Email, err)
	}
	s.Sessions.Issue(w, session.Session{AccountID: a.ID, Email: a.Email, Epoch: a.SessionEpoch})
	if r.FormValue("return_to") != "" {
		s.mergePendingReturnTo(w, r, returnTo)
	}
	http.Redirect(w, r, "/account?notice=trial", http.StatusFound)
}

// mergePendingReturnTo sets return_to on the pending cookie without dropping a pending plan.
func (s *Server) mergePendingReturnTo(w http.ResponseWriter, r *http.Request, returnTo string) {
	pending, err := s.Sessions.Pending(r)
	if err != nil {
		pending = url.Values{}
	}
	pending.Set("return_to", returnTo)
	s.Sessions.SetPending(w, pending, pendingTTL)
}

// sessionConfirm shows a confirm button for a Patreon sign-in token without spending its nonce.
func (s *Server) sessionConfirm(w http.ResponseWriter, r *http.Request) {
	token := r.FormValue("t")
	c, ok := s.verifyHandoff(token, apihandoff.PurposeLogin)
	if !ok {
		s.handoffFailed(w, "Sign-in link expired")
		return
	}
	returnTo := validReturnTo(r.FormValue("return_to"), s.PricingURL)
	p := s.pageFor(nil, "Sign in with Patreon")
	p.Data = sessionConfirmData{Email: apiaccess.NormalizeEmail(c.Email), Name: c.Name, Token: token, ReturnTo: returnTo}
	s.render(w, http.StatusOK, "session_confirm.html", p)
}

// patreonSession signs a Patreon user in, creating the account if needed.
func (s *Server) patreonSession(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		s.fail(w, r, http.StatusForbidden, "That request did not come from this site. Open the link again and use the button on the page.")
		return
	}
	claims, ok := s.handoffClaims(r, apihandoff.PurposeLogin)
	if !ok {
		s.handoffFailed(w, "Sign-in link expired")
		return
	}
	returnTo := validReturnTo(r.FormValue("return_to"), s.PricingURL)
	a, err := s.Store.GetOrCreateAccount(r.Context(), claims.Email, "patreon")
	if err != nil {
		s.logf("session %s: %v", claims.Email, err)
		s.fail(w, r, http.StatusInternalServerError, tryAgainMsg)
		return
	}
	if a.Status != "active" {
		s.fail(w, r, http.StatusForbidden, suspendedMsg)
		return
	}
	s.Sessions.Issue(w, session.Session{AccountID: a.ID, Email: a.Email, Epoch: a.SessionEpoch})
	if r.FormValue("return_to") != "" {
		s.mergePendingReturnTo(w, r, returnTo)
	}
	s.afterLogin(w, r)
}
