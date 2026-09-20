package portal

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/mtgban/api-gatewahy/billing"
	"github.com/mtgban/api-gatewahy/session"
	"github.com/stripe/stripe-go/v84"
)

const starterQuery = "/checkout?package=starter&interval=monthly&games=magic&games=pokemon&stores=CK&stores=SCG&return_to=https%3A%2F%2Fpokemon.mtgban.com%2Fapi-plans"

func TestCheckoutWithoutSessionShowsLoginAndKeepsThePlan(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do("GET", starterQuery, "")
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `name="email"`) || !strings.Contains(body, "TCGplayer plus one store") || !strings.Contains(body, "https://pokemon.mtgban.com/api-login") {
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
	if rec.Code != 200 || !strings.Contains(body, "$500.00") || !strings.Contains(body, "Card Kingdom") || !strings.Contains(body, "pokemon") || !strings.Contains(body, `name="csrf" value="`+csrf+`"`) {
		t.Fatalf("confirm: %d %s", rec.Code, body)
	}
	form := url.Values{"csrf": {csrf}, "package": {"starter"}, "interval": {"monthly"}, "games": {"magic,pokemon"}, "stores": {"CK,SCG"}, "return_to": {"https://pokemon.mtgban.com/api-plans"}}
	rec = ts.do("POST", "/checkout", form.Encode(), ck)
	if rec.Code != 303 || rec.Header().Get("Location") != "https://checkout.stripe.com/c/pay/test" || f.checkouts != 1 {
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
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "Could not start checkout") || !strings.Contains(rec.Body.String(), "$500.00") {
		t.Errorf("failure: %d %s", rec.Code, rec.Body.String())
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
	ts.store.AddEntitlement(context.Background(), entitlementFor(a.ID, "stripe", "BASE_ACCESS"))
	rec = ts.do("GET", "/checkout/success", "", ck)
	if !strings.Contains(rec.Body.String(), "All EU/US stores, no sealed") || !strings.Contains(rec.Body.String(), `action="/account/keys"`) {
		t.Errorf("success with entitlement: %s", rec.Body.String())
	}
	if rec := ts.do("GET", "/checkout/success", ""); rec.Code != 302 {
		t.Errorf("anonymous success: %d", rec.Code)
	}
	if rec := ts.do("GET", "/checkout/cancel", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "Nothing was charged") {
		t.Errorf("cancel: %d", rec.Code)
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
	ts.store.AddEntitlement(context.Background(), entitlementFor(a.ID, "stripe", "BASE_ACCESS"))
	f.sub = &stripe.Subscription{ID: "sub_1", Customer: &stripe.Customer{ID: "cus_test"},
		Metadata: billing.Plan{Package: "all_stores", Interval: "monthly", Games: []string{"magic"}}.Metadata(a.ID)}
	rec := ts.do("GET", changeQuery, "", ck)
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `action="/account/plan"`) || !strings.Contains(body, "(unchanged)") || !strings.Contains(body, "$800.00") {
		t.Fatalf("change confirm: %d %s", rec.Code, body)
	}
}
