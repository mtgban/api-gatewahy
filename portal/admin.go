package portal

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/api-gatewahy/session"
	"github.com/mtgban/mtgban-website/apiproductlist"
)

type adminHomeData struct {
	Query     string
	Accounts  []apiaccess.Account
	Demo      []apiaccess.DemoAccess
	HasStripe bool
	Actions   []apiaccess.AdminAction
}

type adminAccountData struct {
	Account      apiaccess.Account
	Keys         []apiaccess.Key
	Entitlements []adminEntitlement
	Games        []string
	Modes        []string
	Intervals    []apiproductlist.Interval
	NewInvite    string
	StripeURL    string
	Actions      []apiaccess.AdminAction
	Usage        []apiaccess.KeyUsageRow
}

type adminEntitlement struct {
	apiaccess.Entitlement
	View entitlementView
}

type adminUsageData struct {
	Since, Until, Email, Game string
	KeyPrefix                 string
	Rows                      []apiaccess.UsageRow
	// ByAccount reports whether an account filter narrowed the by-key rows.
	ByAccount bool
	ByKey     []apiaccess.KeyUsageRow
	Paths     []apiaccess.PathUsageRow
}

func (s *Server) renderAdminHome(w http.ResponseWriter, r *http.Request, sess session.Session, status int, notice, errMsg string) {
	q := strings.TrimSpace(r.FormValue("q"))
	var (
		accounts []apiaccess.Account
		err      error
	)
	if q == "" {
		accounts, err = s.Store.ListAccounts(r.Context())
	} else {
		accounts, err = s.Store.SearchAccounts(r.Context(), q)
	}
	if err != nil {
		s.logf("admin accounts: %v", err)
		errMsg = tryAgainMsg
	}
	demo, err := s.Store.ListDemoAccess(r.Context())
	if err != nil {
		s.logf("admin demo list: %v", err)
		if errMsg == "" {
			errMsg = tryAgainMsg
		}
	}
	actions, err := s.Store.ListAdminActions(r.Context(), 0, 20)
	if err != nil {
		s.logf("admin actions: %v", err)
		if errMsg == "" {
			errMsg = tryAgainMsg
		}
	}
	p := s.pageFor(&sess, "Admin")
	p.Wide = true
	p.Notice = notice
	p.Error = errMsg
	p.Data = adminHomeData{Query: q, Accounts: accounts, Demo: demo, HasStripe: s.ReconcileAll != nil, Actions: actions}
	s.render(w, status, "admin_home.html", p)
}

func (s *Server) adminHome(w http.ResponseWriter, r *http.Request, sess session.Session, _ apiaccess.Account) {
	s.renderAdminHome(w, r, sess, http.StatusOK, noticeFor(r), "")
}

func (s *Server) adminReconcile(w http.ResponseWriter, r *http.Request, sess session.Session, _ apiaccess.Account) {
	if s.ReconcileAll == nil {
		s.fail(w, r, http.StatusServiceUnavailable, billingOffMsg)
		return
	}
	res, err := s.ReconcileAll(r.Context())
	if err != nil {
		s.renderAdminHome(w, r, sess, http.StatusBadGateway, "", "Reconcile failed: "+err.Error())
		return
	}
	s.audit(r, sess, "reconcile", 0, "", res.Summary())
	s.renderAdminHome(w, r, sess, http.StatusOK, res.Summary(), "")
}

// adminTarget loads the account named by {id}, or writes 404.
func (s *Server) adminTarget(w http.ResponseWriter, r *http.Request) (apiaccess.Account, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return apiaccess.Account{}, false
	}
	a, err := s.Store.GetAccount(r.Context(), id)
	if errors.Is(err, apiaccess.ErrNotFound) {
		http.NotFound(w, r)
		return apiaccess.Account{}, false
	}
	if err != nil {
		s.logf("admin target %d: %v", id, err)
		s.fail(w, r, http.StatusInternalServerError, tryAgainMsg)
		return apiaccess.Account{}, false
	}
	return a, true
}

func (s *Server) renderAdminAccount(w http.ResponseWriter, r *http.Request, sess session.Session, a apiaccess.Account, status int, notice, errMsg, newInvite string) {
	ctx := r.Context()
	d := adminAccountData{Account: a, Games: s.Games, Modes: apiaccess.ValidModes, NewInvite: newInvite}
	if a.StripeCustomerID != "" {
		d.StripeURL = "https://dashboard.stripe.com/customers/" + a.StripeCustomerID
	}
	for _, iv := range s.Catalog.Intervals {
		if !iv.Public {
			d.Intervals = append(d.Intervals, iv)
		}
	}
	keys, err := s.Store.ListKeys(ctx, a.ID)
	if err != nil {
		s.logf("admin account %d: keys: %v", a.ID, err)
		if errMsg == "" {
			errMsg = tryAgainMsg
		}
	}
	d.Keys = keys
	ents, err := s.Store.ListEntitlements(ctx, a.ID)
	if err != nil {
		s.logf("admin account %d: entitlements: %v", a.ID, err)
		if errMsg == "" {
			errMsg = tryAgainMsg
		}
	}
	sites := s.newSiteLookup(r.Context())
	for _, e := range ents {
		d.Entitlements = append(d.Entitlements, adminEntitlement{Entitlement: e, View: s.describeEntitlement(sites, e)})
	}
	actions, err := s.Store.ListAdminActions(ctx, a.ID, 20)
	if err != nil {
		s.logf("admin account %d: actions: %v", a.ID, err)
		if errMsg == "" {
			errMsg = tryAgainMsg
		}
	}
	d.Actions = actions
	now := s.now()
	usage, err := s.Store.UsageByKey(ctx, monthStart(now), now, a.ID)
	if err != nil {
		s.logf("admin account %d: usage: %v", a.ID, err)
		if errMsg == "" {
			errMsg = tryAgainMsg
		}
	}
	d.Usage = usage
	p := s.pageFor(&sess, "Account "+a.Email)
	p.Wide = true
	p.Notice = notice
	p.Error = errMsg
	p.Data = d
	s.render(w, status, "admin_account.html", p)
}

func (s *Server) adminAccount(w http.ResponseWriter, r *http.Request, sess session.Session, _ apiaccess.Account) {
	a, ok := s.adminTarget(w, r)
	if !ok {
		return
	}
	s.renderAdminAccount(w, r, sess, a, http.StatusOK, noticeFor(r), "", "")
}

func (s *Server) adminRedirect(w http.ResponseWriter, r *http.Request, a apiaccess.Account, notice string) {
	http.Redirect(w, r, "/admin/accounts/"+strconv.FormatInt(a.ID, 10)+"?notice="+notice, http.StatusFound)
}

func (s *Server) adminStatus(w http.ResponseWriter, r *http.Request, sess session.Session, _ apiaccess.Account) {
	a, ok := s.adminTarget(w, r)
	if !ok {
		return
	}
	status := apiaccess.AccountStatus(r.FormValue("status"))
	if !status.Valid() {
		s.renderAdminAccount(w, r, sess, a, http.StatusBadRequest, "", "Status must be active or suspended.", "")
		return
	}
	if err := s.Store.SetAccountStatus(r.Context(), a.ID, status); err != nil {
		s.renderAdminAccount(w, r, sess, a, http.StatusInternalServerError, "", tryAgainMsg, "")
		return
	}
	s.audit(r, sess, "status", a.ID, "", string(status))
	s.notify(r)
	s.adminRedirect(w, r, a, "status")
}

func (s *Server) adminNote(w http.ResponseWriter, r *http.Request, sess session.Session, _ apiaccess.Account) {
	a, ok := s.adminTarget(w, r)
	if !ok {
		return
	}
	note := strings.TrimSpace(r.FormValue("note"))
	if err := s.Store.SetAccountNote(r.Context(), a.ID, note); err != nil {
		s.renderAdminAccount(w, r, sess, a, http.StatusInternalServerError, "", tryAgainMsg, "")
		return
	}
	s.audit(r, sess, "note", a.ID, "", note)
	s.adminRedirect(w, r, a, "saved")
}

func (s *Server) adminRevokeKey(w http.ResponseWriter, r *http.Request, sess session.Session, _ apiaccess.Account) {
	a, ok := s.adminTarget(w, r)
	if !ok {
		return
	}
	kid, err := strconv.ParseInt(r.PathValue("kid"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	k, err := s.Store.RevokeKey(r.Context(), kid, a.ID)
	if errors.Is(err, apiaccess.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.renderAdminAccount(w, r, sess, a, http.StatusInternalServerError, "", tryAgainMsg, "")
		return
	}
	if err := s.Store.Notify(r.Context(), k.Hash); err != nil {
		s.logf("notify: %v", err)
	}
	s.audit(r, sess, "key revoke", a.ID, k.Prefix, "")
	s.adminRedirect(w, r, a, "revoked")
}

func (s *Server) adminAddEntitlement(w http.ResponseWriter, r *http.Request, sess session.Session, _ apiaccess.Account) {
	a, ok := s.adminTarget(w, r)
	if !ok {
		return
	}
	bad := func(msg string) { s.renderAdminAccount(w, r, sess, a, http.StatusBadRequest, "", msg, "") }
	in := apiaccess.ManualGrantInput{AccountID: a.ID, Games: listValues(r.PostForm, "games"), Stores: r.FormValue("stores"),
		Modes: listValues(r.PostForm, "modes"), Until: r.FormValue("until"), Note: strings.TrimSpace(r.FormValue("note"))}
	e, err := apiaccess.ManualGrant(in, s.Games, s.now())
	if err != nil {
		var ge *apiaccess.GrantError
		if !errors.As(err, &ge) {
			bad(err.Error())
			return
		}
		switch ge.Field {
		case "games":
			if ge.Msg == "no games given" {
				bad("Pick at least one game.")
				return
			}
			bad("Unknown game " + ge.Value + ".")
		case "stores":
			bad("Stores: " + ge.Msg)
		case "modes":
			bad("Modes: " + ge.Msg)
		case "until":
			if ge.Msg == "until must be in the future" {
				bad("Until must be in the future.")
				return
			}
			bad("Until must be YYYY-MM-DD.")
		default:
			bad(ge.Msg)
		}
		return
	}
	added, err := s.Store.AddEntitlement(r.Context(), e)
	if err != nil {
		s.logf("admin add entitlement %d: %v", a.ID, err)
		s.renderAdminAccount(w, r, sess, a, http.StatusInternalServerError, "", tryAgainMsg, "")
		return
	}
	s.audit(r, sess, "grant", a.ID, "entitlement "+itoa(added.ID), apiaccess.GrantDetail(added))
	s.notify(r)
	s.adminRedirect(w, r, a, "granted")
}

// stripeEndMsg is shown when an admin tries to end a Stripe-sourced row:
// the next reconcile would just restore it from Stripe.
const stripeEndMsg = "Stripe plans are cancelled in Stripe. Suspend the account for an immediate cut-off."

func (s *Server) adminEndEntitlement(w http.ResponseWriter, r *http.Request, sess session.Session, _ apiaccess.Account) {
	a, ok := s.adminTarget(w, r)
	if !ok {
		return
	}
	eid, err := strconv.ParseInt(r.PathValue("eid"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ents, err := s.Store.ListEntitlements(r.Context(), a.ID)
	if err != nil {
		s.logf("admin end entitlement %d: entitlements: %v", eid, err)
		s.renderAdminAccount(w, r, sess, a, http.StatusInternalServerError, "", tryAgainMsg, "")
		return
	}
	i := slices.IndexFunc(ents, func(e apiaccess.Entitlement) bool { return e.ID == eid })
	if i == -1 {
		http.NotFound(w, r)
		return
	}
	if ents[i].Source == apiaccess.SourceStripe {
		s.renderAdminAccount(w, r, sess, a, http.StatusBadRequest, "", stripeEndMsg, "")
		return
	}
	if _, err := s.Store.EndEntitlement(r.Context(), eid, a.ID, s.now()); err != nil {
		if errors.Is(err, apiaccess.ErrStripeEntitlement) {
			s.renderAdminAccount(w, r, sess, a, http.StatusBadRequest, "", stripeEndMsg, "")
			return
		}
		if errors.Is(err, apiaccess.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.logf("admin end entitlement %d: %v", eid, err)
		s.renderAdminAccount(w, r, sess, a, http.StatusInternalServerError, "", tryAgainMsg, "")
		return
	}
	s.audit(r, sess, "end", a.ID, "entitlement "+itoa(eid), "")
	s.notify(r)
	s.adminRedirect(w, r, a, "ended")
}

func (s *Server) adminInvite(w http.ResponseWriter, r *http.Request, sess session.Session, _ apiaccess.Account) {
	a, ok := s.adminTarget(w, r)
	if !ok {
		return
	}
	iv, found := s.Catalog.Interval(r.FormValue("interval"))
	if !found || iv.Public {
		s.renderAdminAccount(w, r, sess, a, http.StatusBadRequest, "", "Pick a non-public interval.", "")
		return
	}
	days, err := strconv.Atoi(r.FormValue("days"))
	if err != nil || days <= 0 {
		s.renderAdminAccount(w, r, sess, a, http.StatusBadRequest, "", "Days must be a positive number.", "")
		return
	}
	if days > 365 {
		s.renderAdminAccount(w, r, sess, a, http.StatusBadRequest, "", "Days must be at most 365.", "")
		return
	}
	token, _, err := s.Store.CreateInvite(r.Context(), iv.Key, a.Email, time.Duration(days)*24*time.Hour, strings.TrimSpace(r.FormValue("note")))
	if err != nil {
		s.renderAdminAccount(w, r, sess, a, http.StatusInternalServerError, "", tryAgainMsg, "")
		return
	}
	s.audit(r, sess, "invite", a.ID, iv.Key, itoa(int64(days))+" days")
	s.renderAdminAccount(w, r, sess, a, http.StatusOK, "Invite created. It is shown once.", "", token)
}

func (s *Server) adminUsage(w http.ResponseWriter, r *http.Request, sess session.Session, _ apiaccess.Account) {
	now := s.now()
	d := adminUsageData{Since: r.FormValue("since"), Until: r.FormValue("until"), Email: strings.TrimSpace(r.FormValue("email")),
		Game: r.FormValue("game"), KeyPrefix: strings.TrimSpace(r.FormValue("key"))}
	from, to, err := apiaccess.UsageWindow(d.Since, d.Until, now)
	if err != nil {
		var we *apiaccess.WindowError
		msg := "Dates must be YYYY-MM-DD."
		if errors.As(err, &we) && we.Field == "order" {
			msg = "Since must not be after until."
		}
		s.fail(w, r, http.StatusBadRequest, msg)
		return
	}
	if d.Since == "" {
		d.Since = from.Format("2006-01-02")
	}
	if d.Until == "" {
		d.Until = to.Format("2006-01-02")
	}
	var accountID int64
	if d.Email != "" {
		a, err := s.Store.GetAccountByEmail(r.Context(), d.Email)
		if err != nil {
			s.fail(w, r, http.StatusNotFound, "No account with that email.")
			return
		}
		accountID = a.ID
	}
	rows, err := s.Store.SummarizeUsage(r.Context(), from, to, accountID)
	if errors.Is(err, apiaccess.ErrNotFound) {
		s.fail(w, r, http.StatusNotFound, tryAgainMsg)
		return
	}
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, tryAgainMsg)
		return
	}
	for _, row := range rows {
		if d.Game == "" || row.Game == d.Game {
			d.Rows = append(d.Rows, row)
		}
	}
	var errMsg string
	var byKey []apiaccess.KeyUsageRow
	// Without an account the by-key query sorts the whole usage table, so ask for one first.
	if accountID != 0 {
		byKey, err = s.Store.UsageByKey(r.Context(), from, to, accountID)
		if err != nil {
			s.logf("admin usage by key: %v", err)
			errMsg = tryAgainMsg
		}
	}
	d.ByAccount = accountID != 0
	d.ByKey = byKey
	if d.KeyPrefix != "" {
		for _, row := range byKey {
			if row.Prefix == d.KeyPrefix {
				paths, err := s.Store.TopPaths(r.Context(), from, to, row.KeyID, 20)
				if err != nil {
					s.logf("admin usage paths: %v", err)
					errMsg = tryAgainMsg
				}
				d.Paths = paths
				break
			}
		}
	}
	p := s.pageFor(&sess, "Usage")
	p.Wide = true
	p.Error = errMsg
	p.Data = d
	s.render(w, http.StatusOK, "admin_usage.html", p)
}

// audit records an admin mutation on the account's activity log.
func (s *Server) audit(r *http.Request, sess session.Session, action string, accountID int64, target, detail string) {
	if err := s.Store.RecordAdminAction(r.Context(), sess.Email, action, accountID, target, detail); err != nil {
		s.logf("audit %s: %v", action, err)
	}
}

// notify asks every gateway to drop its cache; the write already landed, so failure is only logged.
func (s *Server) notify(r *http.Request) {
	if err := s.Store.Notify(r.Context(), ""); err != nil {
		s.logf("notify: %v", err)
	}
}
