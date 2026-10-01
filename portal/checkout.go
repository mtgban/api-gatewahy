package portal

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/api-gatewahy/billing"
	"github.com/mtgban/api-gatewahy/session"
	"github.com/mtgban/mtgban-website/apiproductlist"
)

const billingOffMsg = "Billing is not available right now. Try again later or contact administrator@mtgban.com."
const alreadyHasPlanMsg = "You already have a plan. Use Change plan on your account page to switch."
const manySubscriptionsMsg = "Your account has more than one subscription. Contact administrator@mtgban.com and we will sort it out."
const storesUnavailableMsg = "The store list could not be loaded. Try again in a minute."
const noSubscriptionMsg = "You have no active subscription to change. Start a new plan from the pricing page instead."

// confirmData is confirm.html's payload: the plan in words and the POST fields.
type confirmData struct {
	Package    string
	Games      string
	Stores     string
	Interval   string
	Total      string
	Change     bool
	Invite     string
	ReturnTo   string
	Action     string
	Fields     url.Values
	BillingOff bool
	HasPlan    bool
}

type successData struct {
	Entitlements []entitlementView
	HasKeys      bool
	ReturnTo     string
}

// checkoutGet validates the plan from the query or the pending cookie, then
// shows login or the confirm page.
func (s *Server) checkoutGet(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("package") == "" {
		pv, err := s.Sessions.Pending(r)
		if err != nil || pv.Get("package") == "" {
			http.Redirect(w, r, s.PricingURL, http.StatusFound)
			return
		}
		q = pv
	}
	if q.Get("interval") == "" {
		q.Set("interval", "monthly")
	}
	returnTo := validReturnTo(q.Get("return_to"), s.PricingURL)
	invite := q.Get("invite")
	change := q.Get("change") == "1"
	plan := planFromValues(q)
	if pkg, ok := s.Catalog.Package(plan.Package); ok && pkg.StoreScope != apiproductlist.StoreScopeExplicit {
		plan.Stores = nil
	}
	plan, err := plan.Validate(s.Catalog, s.Games, invite != "")
	var resolved billing.ResolvedPlan
	if err == nil {
		resolved, err = plan.Resolve(r.Context(), s.Catalog, s.Stores)
	}
	if errors.Is(err, billing.ErrStoresUnavailable) {
		s.logf("checkout stores: %v", err)
		s.fail(w, r, http.StatusServiceUnavailable, storesUnavailableMsg)
		return
	}
	if err != nil {
		s.Sessions.ClearPending(w)
		p := s.pageFor(nil, "That plan does not work")
		p.Error = checkoutError(err)
		p.Data = errorData{BackURL: returnTo, BackText: "Back to plans"}
		s.render(w, http.StatusBadRequest, "error.html", p)
		return
	}
	pending := planValues(plan)
	pending.Set("return_to", returnTo)
	if invite != "" {
		pending.Set("invite", invite)
	}
	if change {
		pending.Set("change", "1")
	}
	s.Sessions.SetPending(w, pending, pendingTTL)

	sess, _, err := s.current(r)
	if errors.Is(err, errSuspended) {
		s.fail(w, r, http.StatusForbidden, suspendedMsg)
		return
	}
	if err != nil {
		s.renderLogin(w, r, http.StatusOK, "", pending)
		return
	}
	var hasPlan bool
	if !change {
		hasPlan, err = s.hasActiveStripePlan(r, sess.AccountID)
		if err != nil {
			s.fail(w, r, http.StatusInternalServerError, tryAgainMsg)
			return
		}
	}
	if change {
		var status int
		var msg string
		resolved.Plan, status, msg = s.currentIntervalFor(r, sess, plan)
		if msg != "" {
			s.fail(w, r, status, msg)
			return
		}
	}
	s.renderConfirm(w, r, sess, confirmOptions{Status: http.StatusOK, Resolved: resolved, Invite: invite, ReturnTo: returnTo, Change: change, HasPlan: hasPlan})
}

// hasActiveStripePlan reports whether the account already has an active Stripe entitlement.
func (s *Server) hasActiveStripePlan(r *http.Request, accountID int64) (bool, error) {
	ents, err := s.Store.ListEntitlements(r.Context(), accountID)
	if err != nil {
		return false, err
	}
	return apiaccess.HasActiveStripePlan(ents, s.now()), nil
}

// subscriptionFor finds the account's one Stripe subscription, or the status
// and message to show when there is none to change.
func (s *Server) subscriptionFor(ctx context.Context, accountID int64, email string) (string, int, string) {
	ents, err := s.Store.ListEntitlements(ctx, accountID)
	if err != nil {
		s.logf("plan change %s: entitlements: %v", email, err)
		return "", http.StatusInternalServerError, tryAgainMsg
	}
	subID, err := billing.SubscriptionFor(ents, s.now())
	if errors.Is(err, billing.ErrManySubscriptions) {
		return "", http.StatusBadRequest, manySubscriptionsMsg
	}
	if err != nil {
		return "", http.StatusBadRequest, noSubscriptionMsg
	}
	return subID, 0, ""
}

// currentIntervalFor pins a plan change to the subscription's own interval,
// returning the status and message to show on failure.
func (s *Server) currentIntervalFor(r *http.Request, sess session.Session, plan billing.Plan) (billing.Plan, int, string) {
	if s.Stripe == nil {
		return plan, http.StatusServiceUnavailable, billingOffMsg
	}
	subID, status, msg := s.subscriptionFor(r.Context(), sess.AccountID, sess.Email)
	if msg != "" {
		return plan, status, msg
	}
	sub, err := s.Stripe.GetSubscription(r.Context(), subID)
	if err != nil {
		s.logf("current interval %s: stripe: %v", sess.Email, err)
		return plan, http.StatusBadGateway, tryAgainMsg
	}
	current, _, err := billing.PlanFromMetadata(sub.Metadata)
	if err != nil {
		// Metadata we wrote ourselves at checkout being unreadable is our bug, not Stripe's.
		s.logf("current interval %s: metadata: %v", sess.Email, err)
		return plan, http.StatusInternalServerError, tryAgainMsg
	}
	plan.Interval = current.Interval
	// The interval was authorized when the subscription began, so an invite is not needed again.
	plan, err = plan.Validate(s.Catalog, s.Games, true)
	if err != nil {
		return plan, http.StatusBadRequest, checkoutError(err)
	}
	return plan, 0, ""
}

// confirmOptions is renderConfirm's payload, beside the request and session.
type confirmOptions struct {
	Status   int
	Resolved billing.ResolvedPlan
	Invite   string
	ReturnTo string
	Change   bool
	ErrMsg   string
	HasPlan  bool
}

// renderConfirm draws the plan in words with the POST button.
func (s *Server) renderConfirm(w http.ResponseWriter, r *http.Request, sess session.Session, opt confirmOptions) {
	plan := opt.Resolved.Plan
	pkg, _ := s.Catalog.Package(plan.Package)
	iv, _ := s.Catalog.Interval(plan.Interval)
	total, err := plan.Total(s.Catalog)
	if err != nil {
		s.logf("confirm total for %s: %v", sess.Email, err)
		s.fail(w, r, http.StatusInternalServerError, tryAgainMsg)
		return
	}
	d := confirmData{Package: pkg.Name, Games: strings.Join(plan.Games, ", "), Total: billing.Dollars(total), Change: opt.Change, Invite: opt.Invite, ReturnTo: opt.ReturnTo, Action: "/checkout", BillingOff: s.Stripe == nil, HasPlan: opt.HasPlan}
	if pkg.StoreScope == apiproductlist.StoreScopeExplicit {
		d.Stores = strings.Join(plan.Stores, ", ")
		if len(opt.Resolved.Names) > 0 {
			d.Stores = strings.Join(opt.Resolved.StoreNames(), ", ")
		}
	}
	if iv.Count == 1 {
		d.Interval = "every month"
	} else {
		d.Interval = "every " + itoa(iv.Count) + " months"
	}
	if opt.Change {
		d.Action = "/account/plan"
	}
	d.Fields = planValues(plan)
	d.Fields.Set("return_to", opt.ReturnTo)
	if opt.Invite != "" {
		d.Fields.Set("invite", opt.Invite)
	}
	p := s.pageFor(&sess, "Confirm your plan")
	if opt.Change {
		p.Title = "Confirm the change"
	}
	p.Error = opt.ErrMsg
	p.Data = d
	if s.Stripe == nil && opt.ErrMsg == "" {
		p.Error = billingOffMsg
	}
	s.render(w, opt.Status, "confirm.html", p)
}

// checkoutPost creates the Checkout Session and sends the customer to Stripe.
func (s *Server) checkoutPost(w http.ResponseWriter, r *http.Request, sess session.Session, a apiaccess.Account) {
	if s.Checkout == nil {
		s.fail(w, r, http.StatusServiceUnavailable, billingOffMsg)
		return
	}
	hasPlan, err := s.hasActiveStripePlan(r, a.ID)
	if err != nil {
		s.logf("checkout for %s: has plan: %v", a.Email, err)
		s.fail(w, r, http.StatusInternalServerError, tryAgainMsg)
		return
	}
	if hasPlan {
		s.fail(w, r, http.StatusConflict, alreadyHasPlanMsg)
		return
	}
	invite := r.FormValue("invite")
	returnTo := validReturnTo(r.FormValue("return_to"), s.PricingURL)
	plan, err := planFromValues(r.PostForm).Validate(s.Catalog, s.Games, invite != "")
	var resolved billing.ResolvedPlan
	if err == nil {
		resolved, err = plan.Resolve(r.Context(), s.Catalog, s.Stores)
	}
	if err != nil {
		s.logf("checkout for %s: %v", a.Email, err)
		s.fail(w, r, planErrorStatus(err), checkoutError(err))
		return
	}
	cs, err := s.Checkout.Create(r.Context(), billing.Request{Account: a, Plan: plan, Invite: invite, Resolved: &resolved})
	// The check above is a hint; a plan that lands after it is caught by Create.
	if errors.Is(err, billing.ErrHasPlan) {
		s.fail(w, r, http.StatusConflict, alreadyHasPlanMsg)
		return
	}
	if err != nil {
		s.logf("checkout for %s: %v", a.Email, err)
		s.renderConfirm(w, r, sess, confirmOptions{Status: http.StatusBadGateway, Resolved: resolved, Invite: invite, ReturnTo: returnTo, ErrMsg: checkoutError(err)})
		return
	}
	// Keep the invite and the session id, so a cancelled checkout can expire the session and resume with the invite.
	pending := planValues(plan)
	pending.Set("return_to", returnTo)
	pending.Set("cs", cs.ID)
	if invite != "" {
		pending.Set("invite", invite)
	}
	s.Sessions.SetPending(w, pending, pendingTTL)
	http.Redirect(w, r, cs.URL, http.StatusSeeOther)
}

// checkoutError turns billing errors into sentences for the page.
func checkoutError(err error) string {
	switch {
	case errors.Is(err, billing.ErrInviteRequired):
		return "That billing interval needs an invite from MTGBAN."
	case errors.Is(err, apiaccess.ErrInviteInvalid):
		return "That invite is invalid, already used, or expired."
	case errors.Is(err, billing.ErrPriceNotSeeded):
		return "That plan is not set up for sale yet. Contact administrator@mtgban.com."
	case errors.Is(err, billing.ErrStoresUnavailable):
		return storesUnavailableMsg
	}
	var ve *billing.ValidationError
	if errors.As(err, &ve) {
		// A message that is already a sentence stands on its own.
		if strings.HasSuffix(ve.Msg, ".") {
			return ve.Msg
		}
		return "That plan is not valid: " + ve.Msg + "."
	}
	return "Could not start checkout. Try again in a minute."
}

// success lands the customer after Stripe and offers the first key.
func (s *Server) success(w http.ResponseWriter, r *http.Request, sess session.Session, a apiaccess.Account) {
	returnTo := s.PricingURL
	if pv, err := s.Sessions.Pending(r); err == nil {
		returnTo = validReturnTo(pv.Get("return_to"), s.PricingURL)
	}
	s.Sessions.ClearPending(w)
	d := successData{ReturnTo: returnTo}
	ents, err := s.Store.ListEntitlements(r.Context(), a.ID)
	if err != nil {
		s.logf("success %s: %v", a.Email, err)
	}
	sites := s.newSiteLookup(r.Context())
	for _, e := range ents {
		if e.IsActiveStripePlan(s.now()) {
			d.Entitlements = append(d.Entitlements, s.describeEntitlement(sites, e))
		}
	}
	keys, _ := s.Store.ListKeys(r.Context(), a.ID)
	for _, k := range keys {
		if k.RevokedAt == nil {
			d.HasKeys = true
		}
	}
	p := s.pageFor(&sess, "Payment received")
	p.Data = d
	s.render(w, http.StatusOK, "success.html", p)
}

// cancel is the landing page for an abandoned Checkout Session; no session needed.
func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	returnTo := s.PricingURL
	if pv, err := s.Sessions.Pending(r); err == nil {
		returnTo = validReturnTo(pv.Get("return_to"), s.PricingURL)
		// The invite comes back only once Stripe has expired the session it paid for;
		// otherwise the same invite could pay for that session and a new one.
		if invite := pv.Get("invite"); invite != "" {
			if id := pv.Get("cs"); id != "" && s.Checkout != nil {
				if err := s.Checkout.Abandon(r.Context(), id, invite); err != nil {
					s.logf("cancel abandon %s: %v", id, err)
				}
			}
			pv.Del("invite")
		}
		pv.Del("cs")
		s.Sessions.SetPending(w, pv, pendingTTL)
	}
	var sess *session.Session
	if got, _, err := s.current(r); err == nil {
		sess = &got
	}
	p := s.pageFor(sess, "Checkout cancelled")
	p.Data = errorData{BackURL: returnTo, BackText: "Back to plans"}
	s.render(w, http.StatusOK, "cancel.html", p)
}

// planErrorStatus is 503 while a site's store list is down and 400 for a plan that is wrong.
func planErrorStatus(err error) int {
	if errors.Is(err, billing.ErrStoresUnavailable) {
		return http.StatusServiceUnavailable
	}
	return http.StatusBadRequest
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
