package portal

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/api-gatewahy/billing"
	"github.com/mtgban/api-gatewahy/session"
	"github.com/stripe/stripe-go/v84"
)

const starterQuery = "/checkout?package=starter&interval=monthly&games=magic&games=pokemon&stores=CK&stores=SCG&return_to=https%3A%2F%2Fpokemon.mtgban.com%2Fapi-plans"

func TestCheckoutWithoutSessionShowsLoginAndKeepsThePlan(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do("GET", starterQuery, "")
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `name="email"`) || !strings.Contains(body, "À la carte") || !strings.Contains(body, "https://pokemon.mtgban.com/api-login") {
		t.Fatalf("%d %s", rec.Code, body)
	}
	ck := cookieNamed(rec, session.PendingName)
	if ck == nil {
		t.Fatal("no pending cookie")
	}
	pv, err := ts.Sessions.Open(ck.Value)
	if err != nil || pv.Get("package") != "starter" || pv.Get("stores") != "CK,SCG" || pv.Get("return_to") != "https://pokemon.mtgban.com/api-plans" {
		t.Errorf("pending %v %v", pv, err)
	}
}

func TestCheckoutRejectsAnInvalidPlan(t *testing.T) {
	ts := newTestServer(t)
	for _, q := range []string{"/checkout?package=nope&games=magic", "/checkout?package=starter&games=magic", "/checkout?package=all_data&games=chess", "/checkout?package=starter&games=magic&stores=CK&interval=quarterly"} {
		if rec := ts.do("GET", q, ""); rec.Code != 400 {
			t.Errorf("%s: %d", q, rec.Code)
		}
	}
	if rec := ts.do("GET", "/checkout", ""); rec.Code != 302 || rec.Header().Get("Location") != ts.PricingURL {
		t.Errorf("empty: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestCheckoutConfirmAndPost(t *testing.T) {
	ts := newTestServer(t)
	f := ts.withStripe()
	_, ck, csrf := ts.signIn(t, "ann@example.com")
	rec := ts.do("GET", starterQuery, "", ck)
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "$500") || !strings.Contains(body, "Card Kingdom") || !strings.Contains(body, "pokemon") || !strings.Contains(body, `name="csrf" value="`+csrf+`"`) {
		t.Fatalf("confirm: %d %s", rec.Code, body)
	}
	form := url.Values{"csrf": {csrf}, "package": {"starter"}, "interval": {"monthly"}, "games": {"magic,pokemon"}, "stores": {"CK,SCG"}, "return_to": {"https://pokemon.mtgban.com/api-plans"}}
	rec = ts.do("POST", "/checkout", form.Encode(), ck)
	if rec.Code != 303 || rec.Header().Get("Location") != "https://checkout.stripe.com/c/pay/cs_test_1" || f.checkouts != 1 {
		t.Fatalf("post: %d %q %d", rec.Code, rec.Header().Get("Location"), f.checkouts)
	}
	pending := cookieNamed(rec, session.PendingName)
	pv, _ := ts.Sessions.Open(pending.Value)
	if pv.Get("return_to") != "https://pokemon.mtgban.com/api-plans" || pv.Get("package") != "starter" || pv.Get("invite") != "" {
		t.Errorf("pending after post %v", pv)
	}
	a, _ := ts.store.GetAccountByEmail(context.Background(), "ann@example.com")
	if a.StripeCustomerID != "cus_test" {
		t.Error("customer not stored")
	}
	if rec := ts.do("POST", "/checkout", strings.Replace(form.Encode(), csrf, "bad", 1), ck); rec.Code != 403 {
		t.Errorf("csrf: %d", rec.Code)
	}
	f.fail = errors.New("stripe down")
	rec = ts.do("POST", "/checkout", form.Encode(), ck)
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "Could not start checkout") || !strings.Contains(rec.Body.String(), "$500") {
		t.Errorf("failure: %d %s", rec.Code, rec.Body.String())
	}
}

func TestCheckoutBlocksSecondSubscription(t *testing.T) {
	ts := newTestServer(t)
	ts.withStripe()
	a, ck, csrf := ts.signIn(t, "ann@example.com")
	_, _ = ts.store.AddEntitlement(context.Background(), entitlementFor(a.ID, "stripe", "BASE_ACCESS"))

	rec := ts.do("GET", starterQuery, "", ck)
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "You already have a plan") || strings.Contains(body, `class="btn"`) {
		t.Fatalf("confirm with existing plan: %d %s", rec.Code, body)
	}

	form := url.Values{"csrf": {csrf}, "package": {"starter"}, "interval": {"monthly"}, "games": {"magic,pokemon"}, "stores": {"CK,SCG"}}
	rec = ts.do("POST", "/checkout", form.Encode(), ck)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "You already have a plan") {
		t.Errorf("post with existing plan: %d %s", rec.Code, rec.Body.String())
	}
}

func TestManySubscriptionsShowsContactMessage(t *testing.T) {
	ts := newTestServer(t)
	ts.withStripe()
	a, ck, csrf := ts.signIn(t, "ann@example.com")
	ctx := context.Background()
	_, _ = ts.store.AddEntitlement(ctx, entitlementFor(a.ID, "stripe", "BASE_ACCESS"))
	e2 := entitlementFor(a.ID, "stripe", "BASE_ACCESS")
	e2.ExternalRef = "sub_2"
	_, _ = ts.store.AddEntitlement(ctx, e2)

	const changeQuery = "/checkout?change=1&package=all_data&games=magic&return_to=https%3A%2F%2Fmtgban.com%2Fapi-plans"
	rec := ts.do("GET", changeQuery, "", ck)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "more than one subscription") {
		t.Errorf("checkout change: %d %s", rec.Code, rec.Body.String())
	}

	rec = ts.do("POST", "/account/plan", "csrf="+csrf+"&package=all_data&interval=monthly&games=magic", ck)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "more than one subscription") {
		t.Errorf("account plan: %d %s", rec.Code, rec.Body.String())
	}
}

func TestCancelReleasesInvite(t *testing.T) {
	ts := newTestServer(t)
	f := ts.withStripe()
	_, ck, csrf := ts.signIn(t, "ann@example.com")
	ctx := context.Background()
	token, _, err := ts.store.CreateInvite(ctx, "quarterly", "ann@example.com", time.Hour, "")
	if err != nil {
		t.Fatal(err)
	}
	query := "/checkout?package=starter&interval=quarterly&games=magic&games=pokemon&stores=CK&stores=SCG&invite=" + token
	if rec := ts.do("GET", query, "", ck); rec.Code != 200 {
		t.Fatalf("confirm: %d", rec.Code)
	}
	form := url.Values{"csrf": {csrf}, "package": {"starter"}, "interval": {"quarterly"}, "games": {"magic,pokemon"}, "stores": {"CK,SCG"}, "invite": {token}}
	rec := ts.do("POST", "/checkout", form.Encode(), ck)
	if rec.Code != 303 {
		t.Fatalf("post: %d %s", rec.Code, rec.Body.String())
	}
	pending := cookieNamed(rec, session.PendingName)
	if rec := ts.do("GET", "/checkout/cancel", "", ck, pending); rec.Code != 200 {
		t.Fatalf("cancel: %d", rec.Code)
	}
	inv, ok := ts.store.invites[apiaccess.HashKey(token)]
	if !ok || inv.UsedAt != nil {
		t.Errorf("invite not released: %+v", inv)
	}
	if f.sessions["cs_test_1"] != stripe.CheckoutSessionStatusExpired {
		t.Errorf("session not expired at Stripe: %v", f.sessions)
	}
	if rec := ts.do("GET", query, "", ck); rec.Code != 200 {
		t.Errorf("resume checkout: %d %s", rec.Code, rec.Body.String())
	}
}

func TestCheckoutGetFailsClosedOnEntitlementListError(t *testing.T) {
	ts := newTestServer(t)
	ts.withStripe()
	_, ck, _ := ts.signIn(t, "ann@example.com")
	ts.store.listEntitlementsErr = errors.New("db down")
	rec := ts.do("GET", starterQuery, "", ck)
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), tryAgainMsg) {
		t.Errorf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestCheckoutDropsStoresForNonExplicitPackage(t *testing.T) {
	ts := newTestServer(t)
	_, ck, _ := ts.signIn(t, "ann@example.com")
	rec := ts.do("GET", "/checkout?package=all_data&games=magic&stores=CK", "", ck)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "does not take a store list") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestCheckoutWithoutStripeSaysSo(t *testing.T) {
	ts := newTestServer(t)
	_, ck, csrf := ts.signIn(t, "ann@example.com")
	rec := ts.do("POST", "/checkout", "csrf="+csrf+"&package=all_data&interval=monthly&games=magic", ck)
	if rec.Code != 503 {
		t.Errorf("%d", rec.Code)
	}
}

func TestSuccessAndCancelPages(t *testing.T) {
	ts := newTestServer(t)
	a, ck, _ := ts.signIn(t, "ann@example.com")
	pending := cookieFor(ts, map[string][]string{"return_to": {"https://pokemon.mtgban.com/api-plans"}})
	rec := ts.do("GET", "/checkout/success", "", ck, pending)
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "being set up") || !strings.Contains(body, `href="https://pokemon.mtgban.com/api-plans"`) {
		t.Fatalf("success empty: %d %s", rec.Code, body)
	}
	if c := cookieNamed(rec, session.PendingName); c == nil || c.MaxAge >= 0 {
		t.Error("pending cookie not cleared")
	}
	_, _ = ts.store.AddEntitlement(context.Background(), entitlementFor(a.ID, "stripe", "BASE_ACCESS"))
	rec = ts.do("GET", "/checkout/success", "", ck)
	if !strings.Contains(rec.Body.String(), "Base Access") || !strings.Contains(rec.Body.String(), `action="/account/keys"`) {
		t.Errorf("success with entitlement: %s", rec.Body.String())
	}
	if rec := ts.do("GET", "/checkout/success", ""); rec.Code != 302 {
		t.Errorf("anonymous success: %d", rec.Code)
	}
	if rec := ts.do("GET", "/checkout/cancel", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "Nothing was charged") {
		t.Errorf("cancel: %d", rec.Code)
	}
}

func TestConfirmDisablesButtonWithoutBilling(t *testing.T) {
	ts := newTestServer(t)
	_, ck, _ := ts.signIn(t, "ann@example.com")
	rec := ts.do("GET", starterQuery, "", ck)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `class="btn" disabled>`) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestCheckoutErrorMessages(t *testing.T) {
	ve := &billing.ValidationError{Msg: `unknown package "nope"`}
	if got := checkoutError(ve); got != `That plan is not valid: unknown package "nope".` {
		t.Errorf("validation: %q", got)
	}
	if got := checkoutError(fmt.Errorf("checkout: %w", errors.New("stripe down"))); got != "Could not start checkout. Try again in a minute." {
		t.Errorf("wrapped stripe error: %q", got)
	}
}

func TestCheckoutChangeConfirm(t *testing.T) {
	ts := newTestServer(t)
	_, ck, _ := ts.signIn(t, "ann@example.com")
	const changeQuery = "/checkout?change=1&package=all_data&games=magic&return_to=https%3A%2F%2Fmtgban.com%2Fapi-plans"
	if rec := ts.do("GET", changeQuery, "", ck); rec.Code != 503 {
		t.Errorf("without stripe: %d", rec.Code)
	}
	f := ts.withStripe()
	if rec := ts.do("GET", changeQuery, "", ck); rec.Code != 400 || !strings.Contains(rec.Body.String(), "no active subscription") {
		t.Errorf("no entitlement: %d %s", rec.Code, rec.Body.String())
	}
	a, _ := ts.store.GetAccountByEmail(context.Background(), "ann@example.com")
	_, _ = ts.store.AddEntitlement(context.Background(), entitlementFor(a.ID, "stripe", "BASE_ACCESS"))
	f.sub = &stripe.Subscription{ID: "sub_1", Customer: &stripe.Customer{ID: "cus_test"},
		Metadata: billing.Plan{Package: "all_stores", Interval: "monthly", Games: []string{"magic"}}.Metadata(a.ID)}
	rec := ts.do("GET", changeQuery, "", ck)
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `action="/account/plan"`) || !strings.Contains(body, "(unchanged)") || !strings.Contains(body, "$800") {
		t.Fatalf("change confirm: %d %s", rec.Code, body)
	}
}

// startInviteCheckout signs in, creates a quarterly invite, and posts a checkout with it.
func startInviteCheckout(t *testing.T, ts *testServer) (token string, ck, pending *http.Cookie) {
	t.Helper()
	_, ck, csrf := ts.signIn(t, "ann@example.com")
	token, _, err := ts.store.CreateInvite(context.Background(), "quarterly", "ann@example.com", time.Hour, "")
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"csrf": {csrf}, "package": {"starter"}, "interval": {"quarterly"}, "games": {"magic"}, "stores": {"CK"}, "invite": {token}}
	rec := ts.do("POST", "/checkout", form.Encode(), ck)
	if rec.Code != 303 {
		t.Fatalf("post: %d %s", rec.Code, rec.Body.String())
	}
	return token, ck, cookieNamed(rec, session.PendingName)
}

func TestCancelKeepsInviteWhenSessionCompleted(t *testing.T) {
	ts := newTestServer(t)
	f := ts.withStripe()
	token, ck, pending := startInviteCheckout(t, ts)
	// The customer paid in the Stripe tab, then hit the cancel URL anyway.
	f.sessions["cs_test_1"] = stripe.CheckoutSessionStatusComplete
	if rec := ts.do("GET", "/checkout/cancel", "", ck, pending); rec.Code != 200 {
		t.Fatalf("cancel: %d", rec.Code)
	}
	if inv := ts.store.invites[apiaccess.HashKey(token)]; inv.UsedAt == nil {
		t.Error("invite released although the session completed")
	}
}

func TestCancelWithoutSessionIDKeepsInvite(t *testing.T) {
	ts := newTestServer(t)
	ts.withStripe()
	token, ck, _ := startInviteCheckout(t, ts)
	// A pending cookie that names the invite but not the session cannot prove the session is dead.
	stale := cookieFor(ts, map[string][]string{"invite": {token}, "return_to": {"https://mtgban.com/api-plans"}})
	if rec := ts.do("GET", "/checkout/cancel", "", ck, stale); rec.Code != 200 {
		t.Fatalf("cancel: %d", rec.Code)
	}
	if inv := ts.store.invites[apiaccess.HashKey(token)]; inv.UsedAt == nil {
		t.Error("invite released without expiring its session")
	}
}
