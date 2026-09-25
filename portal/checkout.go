package portal

import (
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

func (s *Server) registerCheckout(mux *http.ServeMux) {
	mux.HandleFunc("GET /checkout", s.checkoutGet)
	mux.HandleFunc("POST /checkout", s.withSession(s.checkoutPost))
	mux.HandleFunc("GET "+s.SuccessPath, s.withSession(s.success))
	mux.HandleFunc("GET "+s.CancelPath, s.cancel)
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
		plan, status, msg = s.currentIntervalFor(r, sess, plan)
		if msg != "" {
			s.fail(w, r, status, msg)
			return
		}
	}
	s.renderConfirm(w, r, http.StatusOK, sess, plan, invite, returnTo, change, "", hasPlan)
}

// hasActiveStripePlan reports whether the account already has an active Stripe entitlement.
func (s *Server) hasActiveStripePlan(r *http.Request, accountID int64) (bool, error) {
	ents, err := s.Store.ListEntitlements(r.Context(), accountID)
	if err != nil {
		return false, err
	}
	return apiaccess.HasActiveStripePlan(ents, s.now()), nil
}

// currentIntervalFor pins a plan change to the subscription's own interval,
// returning the status and message to show on failure.
func (s *Server) currentIntervalFor(r *http.Request, sess session.Session, plan billing.Plan) (billing.Plan, int, string) {
	if s.Stripe == nil {
		return plan, http.StatusServiceUnavailable, billingOffMsg
	}
	ents, err := s.Store.ListEntitlements(r.Context(), sess.AccountID)
	if err != nil {
		return plan, http.StatusBadRequest, tryAgainMsg
	}
	subID, err := billing.SubscriptionFor(ents)
	if errors.Is(err, billing.ErrManySubscriptions) {
		return plan, http.StatusBadRequest, manySubscriptionsMsg
	}
	if err != nil {
		return plan, http.StatusBadRequest, "You have no active subscription to change. Start a new plan from the pricing page instead."
	}
	sub, err := s.Stripe.GetSubscription(r.Context(), subID)
	if err != nil {
		return plan, http.StatusBadRequest, tryAgainMsg
	}
	current, _, err := billing.PlanFromMetadata(sub.Metadata)
	if err != nil {
		return plan, http.StatusBadRequest, tryAgainMsg
	}
	plan.Interval = current.Interval
	// The interval was authorized when the subscription began, so an invite is not needed again.
	plan, err = plan.Validate(s.Catalog, s.Games, true)
	if err != nil {
		return plan, http.StatusBadRequest, checkoutError(err)
	}
	return plan, 0, ""
}

// renderConfirm draws the plan in words with the POST button.
func (s *Server) renderConfirm(w http.ResponseWriter, r *http.Request, status int, sess session.Session, plan billing.Plan, invite, returnTo string, change bool, errMsg string, hasPlan bool) {
	pkg, _ := s.Catalog.Package(plan.Package)
	iv, _ := s.Catalog.Interval(plan.Interval)
	total, err := plan.Total(s.Catalog)
	if err != nil {
		s.logf("confirm total for %s: %v", sess.Email, err)
		s.fail(w, r, http.StatusInternalServerError, tryAgainMsg)
		return
	}
	d := confirmData{Package: pkg.Name, Games: strings.Join(plan.Games, ", "), Total: billing.Dollars(total), Change: change, Invite: invite, ReturnTo: returnTo, Action: "/checkout", BillingOff: s.Stripe == nil, HasPlan: hasPlan}
	if pkg.StoreScope == apiproductlist.StoreScopeExplicit {
		d.Stores = storeNamesByKey(s.Catalog, plan.StoreKeys(s.Catalog))
	}
	if iv.Count == 1 {
		d.Interval = "every month"
	} else {
		d.Interval = "every " + itoa(iv.Count) + " months"
	}
	if change {
		d.Action = "/account/plan"
	}
	d.Fields = planValues(plan)
	d.Fields.Set("return_to", returnTo)
	if invite != "" {
		d.Fields.Set("invite", invite)
	}
	p := s.pageFor(&sess, "Confirm your plan")
	if change {
		p.Title = "Confirm the change"
	}
	p.Error = errMsg
	p.Data = d
	if s.Stripe == nil && errMsg == "" {
		p.Error = billingOffMsg
	}
	s.render(w, status, "confirm.html", p)
}

// checkoutPost creates the Checkout Session and sends the customer to Stripe.
func (s *Server) checkoutPost(w http.ResponseWriter, r *http.Request, sess session.Session, a apiaccess.Account) {
	if s.Checkout == nil {
		s.fail(w, r, http.StatusServiceUnavailable, billingOffMsg)
		return
	}
	hasPlan, err := s.hasActiveStripePlan(r, a.ID)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, tryAgainMsg)
		return
	}
	if hasPlan {
		s.fail(w, r, http.StatusConflict, alreadyHasPlanMsg)
		return
	}
	invite := r.FormValue("invite")
	returnTo := validReturnTo(r.FormValue("return_to"), s.PricingURL)
	plan, err := planFromValues(r.Form).Validate(s.Catalog, s.Games, invite != "")
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, checkoutError(err))
		return
	}
	cs, err := s.Checkout.Create(r.Context(), billing.Request{Account: a, Plan: plan, Invite: invite})
	if err != nil {
		s.logf("checkout for %s: %v", a.Email, err)
		s.renderConfirm(w, r, http.StatusBadGateway, sess, plan, invite, returnTo, false, checkoutError(err), false)
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
	}
	var ve *billing.ValidationError
	if errors.As(err, &ve) {
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
	for _, e := range ents {
		if e.IsActiveStripePlan(s.now()) {
			d.Entitlements = append(d.Entitlements, s.describeEntitlement(e))
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

// storeNamesByKey maps catalog store keys to names.
func storeNamesByKey(cat *apiproductlist.ProductList, keys []string) string {
	var names []string
	for _, k := range keys {
		if st, ok := cat.Store(k); ok {
			names = append(names, st.Name)
		} else {
			names = append(names, k)
		}
	}
	return strings.Join(names, ", ")
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
