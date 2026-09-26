package portal

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/api-gatewahy/billing"
	"github.com/mtgban/api-gatewahy/session"
)

// accountData is account.html's payload.
type accountData struct {
	Account      apiaccess.Account
	Entitlements []entitlementView
	Keys         []keyView
	NewKey       string
	Usage        []apiaccess.UsageRow
	Month        string
	HasStripe    bool
	ChangeURL    string
	TrialEnds    string
	ReturnTo     string
}

type keyView struct {
	ID       int64
	Prefix   string
	Kind     string
	Label    string
	Created  string
	LastUsed string
}

func (s *Server) registerAccount(mux *http.ServeMux) {
	mux.HandleFunc("GET /account", s.withSession(s.account))
	mux.HandleFunc("POST /account/keys", s.withSession(s.createKey))
	mux.HandleFunc("POST /account/keys/{id}/revoke", s.withSession(s.revokeKey))
	mux.HandleFunc("POST /account/plan", s.withSession(s.changePlan))
	mux.HandleFunc("GET /portal", s.withSession(s.portal))
}

func (s *Server) account(w http.ResponseWriter, r *http.Request, sess session.Session, a apiaccess.Account) {
	s.renderAccount(w, r, http.StatusOK, sess, a, "", noticeFor(r), "")
}

// renderAccount gathers everything the page shows. newKey is shown once, in this response only.
func (s *Server) renderAccount(w http.ResponseWriter, r *http.Request, status int, sess session.Session, a apiaccess.Account, newKey, notice, errMsg string) {
	ctx := r.Context()
	now := s.now().UTC()
	d := accountData{Account: a, NewKey: newKey, HasStripe: a.StripeCustomerID != "" && s.Stripe != nil, Month: now.Format("January 2006")}
	if pv, err := s.Sessions.Pending(r); err == nil {
		if rt := pv.Get("return_to"); rt != "" {
			d.ReturnTo = validReturnTo(rt, s.PricingURL)
		}
	}
	ents, err := s.Store.ListEntitlements(ctx, a.ID)
	if err != nil {
		s.logf("account %s: entitlements: %v", a.Email, err)
	}
	for _, e := range ents {
		if !e.ActiveAt(now) {
			continue
		}
		d.Entitlements = append(d.Entitlements, s.describeEntitlement(e))
		if e.Source == "trial" && e.ValidUntil != nil {
			d.TrialEnds = e.ValidUntil.Format("January 2, 2006")
		}
		if e.Source == "stripe" && d.ChangeURL == "" && s.Stripe != nil {
			d.ChangeURL = mergeQuery(s.PricingURL, prefillQuery(s.Catalog, e))
		}
	}
	keys, err := s.Store.ListKeys(ctx, a.ID)
	if err != nil {
		s.logf("account %s: keys: %v", a.Email, err)
	}
	for _, k := range keys {
		if k.RevokedAt != nil {
			continue
		}
		kv := keyView{ID: k.ID, Prefix: k.Prefix, Kind: string(k.Kind), Label: k.Label, Created: k.CreatedAt.Format("2006-01-02"), LastUsed: "never"}
		if k.LastUsedAt != nil {
			kv.LastUsed = k.LastUsedAt.Format("2006-01-02")
		}
		d.Keys = append(d.Keys, kv)
	}
	if rows, err := s.Store.SummarizeUsage(ctx, monthStart(now), now.Add(time.Second), a.ID); err == nil {
		d.Usage = rows
	} else {
		s.logf("account %s: usage: %v", a.Email, err)
	}
	p := s.pageFor(&sess, "Your API account")
	p.Notice = notice
	p.Error = errMsg
	p.Data = d
	s.render(w, status, "account.html", p)
}

// mergeQuery merges q into pricingURL's existing query string.
func mergeQuery(pricingURL string, q url.Values) string {
	u, err := url.Parse(pricingURL)
	if err != nil {
		return pricingURL + "?" + q.Encode()
	}
	existing := u.Query()
	for k, vs := range q {
		existing[k] = vs
	}
	u.RawQuery = existing.Encode()
	return u.String()
}

// keysPerHour caps how many keys one account can mint in a sliding hour.
const keysPerHour = 10

// maxActiveKeys caps unrevoked keys per account; one per integration is plenty.
const maxActiveKeys = 5

// createKey mints a key and shows it once.
func (s *Server) createKey(w http.ResponseWriter, r *http.Request, sess session.Session, a apiaccess.Account) {
	if !s.limit.allow("keys:"+itoa(a.ID), keysPerHour, s.now()) {
		s.renderAccount(w, r, http.StatusTooManyRequests, sess, a, "", "", "Too many keys created in the last hour. Try again later.")
		return
	}
	label := strings.TrimSpace(r.FormValue("label"))
	if runes := []rune(label); len(runes) > 64 {
		label = string(runes[:64])
	}
	if label == "" {
		s.renderAccount(w, r, http.StatusBadRequest, sess, a, "", "", "Give the key a label, such as the machine or spreadsheet that will use it.")
		return
	}
	keys, err := s.Store.ListKeys(r.Context(), a.ID)
	if err != nil {
		s.logf("list keys %s: %v", a.Email, err)
		s.renderAccount(w, r, http.StatusInternalServerError, sess, a, "", "", tryAgainMsg)
		return
	}
	active := 0
	for _, k := range keys {
		if k.RevokedAt == nil {
			active++
		}
	}
	if active >= maxActiveKeys {
		s.renderAccount(w, r, http.StatusBadRequest, sess, a, "", "", "This account already has "+itoa(int64(maxActiveKeys))+" keys. Revoke one you no longer use before creating another.")
		return
	}
	hasPlan, err := s.hasActiveStripePlan(r, a.ID)
	if err != nil {
		s.logf("key kind %s: %v", a.Email, err)
		s.renderAccount(w, r, http.StatusInternalServerError, sess, a, "", "", tryAgainMsg)
		return
	}
	kind := apiaccess.KeyDemo
	if hasPlan {
		kind = apiaccess.KeyLive
	}
	plain, k, err := s.Store.CreateKey(r.Context(), a.ID, label, kind)
	if err != nil {
		s.logf("create key %s: %v", a.Email, err)
		s.renderAccount(w, r, http.StatusInternalServerError, sess, a, "", "", tryAgainMsg)
		return
	}
	subject, text, htmlBody := keyCreatedMail(k.Prefix, k.Label, s.PublicURL+"/account")
	if err := s.Mail.Send(r.Context(), a.Email, subject, text, htmlBody); err != nil {
		s.logf("key mail %s: %v", a.Email, err)
	}
	s.renderAccount(w, r, http.StatusOK, sess, a, plain, "", "")
}

// revokeKey revokes one of the account's own keys.
func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request, sess session.Session, a apiaccess.Account) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	k, err := s.Store.RevokeKey(r.Context(), id, a.ID)
	if errors.Is(err, apiaccess.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.logf("revoke key %d: %v", id, err)
		s.renderAccount(w, r, http.StatusInternalServerError, sess, a, "", "", tryAgainMsg)
		return
	}
	if err := s.Store.Notify(r.Context(), k.Hash); err != nil {
		s.logf("notify after revoke: %v", err)
	}
	http.Redirect(w, r, "/account?notice=revoked", http.StatusFound)
}

// portal sends the customer to a fresh Stripe Customer Portal session.
func (s *Server) portal(w http.ResponseWriter, r *http.Request, sess session.Session, a apiaccess.Account) {
	if s.Stripe == nil {
		s.fail(w, r, http.StatusServiceUnavailable, billingOffMsg)
		return
	}
	u, err := billing.PortalURL(r.Context(), s.Stripe, a, s.PublicURL+"/account")
	if errors.Is(err, billing.ErrNoCustomer) {
		s.renderAccount(w, r, http.StatusBadRequest, sess, a, "", "", "This account has no billing on file yet. Pick a plan first.")
		return
	}
	if err != nil {
		s.logf("portal %s: %v", a.Email, err)
		s.renderAccount(w, r, http.StatusBadGateway, sess, a, "", "", tryAgainMsg)
		return
	}
	http.Redirect(w, r, u, http.StatusSeeOther)
}

// changePlan rewrites the subscription to the posted plan with proration.
func (s *Server) changePlan(w http.ResponseWriter, r *http.Request, sess session.Session, a apiaccess.Account) {
	if s.Stripe == nil || s.Reconcile == nil {
		s.fail(w, r, http.StatusServiceUnavailable, billingOffMsg)
		return
	}
	_ = r.ParseForm()
	plan, err := planFromValues(r.PostForm).Validate(s.Catalog, s.Games, true)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, checkoutError(err))
		return
	}
	ents, err := s.Store.ListEntitlements(r.Context(), a.ID)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, tryAgainMsg)
		return
	}
	subID, err := billing.SubscriptionFor(ents)
	if errors.Is(err, billing.ErrManySubscriptions) {
		s.fail(w, r, http.StatusBadRequest, manySubscriptionsMsg)
		return
	}
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "You have no active subscription to change. Start a new plan from the pricing page instead.")
		return
	}
	if _, err := billing.ChangePlan(r.Context(), s.Stripe, s.Catalog, s.Games, a, subID, plan, s.Reconcile); err != nil {
		s.logf("plan change %s: %v", a.Email, err)
		s.renderConfirm(w, r, http.StatusBadGateway, sess, plan, "", s.PricingURL, true, checkoutError(err), false)
		return
	}
	http.Redirect(w, r, "/account?notice=plan", http.StatusFound)
}
