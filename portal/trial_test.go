package portal

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/api-gatewahy/session"
	"github.com/mtgban/mtgban-website/apihandoff"
)

// handoff mints a token for purpose/email with a fresh nonce.
func (ts *testServer) handoff(purpose, email string) string {
	nonce, err := apihandoff.NewNonce()
	if err != nil {
		panic(err)
	}
	return apihandoff.Mint(ts.TrialSecret, apihandoff.Claims{Email: email, Name: "Ann Example", Purpose: purpose, Nonce: nonce, Expires: ts.now.Add(apihandoff.TTL)})
}

func TestTrialGrantsOnceAndSignsIn(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()

	tok := ts.handoff(apihandoff.PurposeTrial, "Ann@Example.com")
	rec := ts.do("GET", "/trial?t="+tok+"&return_to=https%3A%2F%2Fpokemon.mtgban.com%2Fapi-plans", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "ann@example.com") || !strings.Contains(rec.Body.String(), tok) ||
		!strings.Contains(rec.Body.String(), `name="return_to" value="https://pokemon.mtgban.com/api-plans"`) ||
		!strings.Contains(rec.Body.String(), "Ann Example") {
		t.Fatalf("confirm: %d %s", rec.Code, rec.Body.String())
	}

	rec = ts.do("POST", "/trial", "t="+tok+"&return_to=https%3A%2F%2Fpokemon.mtgban.com%2Fapi-plans")
	ck := cookieNamed(rec, session.CookieName)
	if rec.Code != 302 || rec.Header().Get("Location") != "/account?notice=trial" || ck == nil {
		t.Fatalf("%d %q", rec.Code, rec.Header().Get("Location"))
	}
	pending := cookieNamed(rec, session.PendingName)
	if rec := ts.do("GET", "/account", "", ck, pending); !strings.Contains(rec.Body.String(), "https://pokemon.mtgban.com/api-plans") {
		t.Errorf("account page missing return_to: %s", rec.Body.String())
	}
	a, err := ts.store.GetAccountByEmail(ctx, "ann@example.com")
	if err != nil {
		t.Fatal("no account")
	}
	ents, _ := ts.store.ListEntitlements(ctx, a.ID)
	if len(ents) != 1 || ents[0].Source != "trial" || ents[0].StoreScope != apiaccess.ScopeAll || len(ents[0].Modes) != 3 || strings.Join(ents[0].Games, ",") != "magic,pokemon" || ents[0].ValidUntil == nil {
		t.Fatalf("entitlement %+v", ents)
	}
	if got := ents[0].ValidUntil.Sub(ts.now); got != 15*24*time.Hour {
		t.Errorf("trial length %v", got)
	}
	if !strings.Contains(ts.mail.String(), "trial has started") || len(ts.store.notified) == 0 {
		t.Error("no trial mail or no notify")
	}

	rec = ts.do("POST", "/trial", "t="+tok)
	if rec.Code != 400 {
		t.Errorf("replay: %d", rec.Code)
	}

	tok2 := ts.handoff(apihandoff.PurposeTrial, "ann@example.com")
	rec = ts.do("GET", "/trial?t="+tok2, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "ann@example.com") {
		t.Errorf("second confirm: %d %s", rec.Code, rec.Body.String())
	}
	rec = ts.do("POST", "/trial", "t="+tok2)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "already used") || !strings.Contains(rec.Body.String(), `href="/login"`) {
		t.Errorf("second trial: %d %s", rec.Code, rec.Body.String())
	}
	if ents, _ = ts.store.ListEntitlements(ctx, a.ID); len(ents) != 1 {
		t.Error("second trial added an entitlement")
	}

	// A foreign return_to falls back to PricingURL.
	tok3 := ts.handoff(apihandoff.PurposeTrial, "foreign@example.com")
	rec = ts.do("GET", "/trial?t="+tok3+"&return_to=https%3A%2F%2Fevil.example%2F", "")
	if !strings.Contains(rec.Body.String(), `name="return_to" value="`+ts.PricingURL+`"`) {
		t.Errorf("foreign return_to not rebased to pricing url: %s", rec.Body.String())
	}
}

func TestTrialWithoutReturnToSetsNoPendingCookie(t *testing.T) {
	ts := newTestServer(t)
	tok := ts.handoff(apihandoff.PurposeTrial, "noreturn@example.com")
	rec := ts.do("POST", "/trial", "t="+tok)
	ck := cookieNamed(rec, session.CookieName)
	if rec.Code != 302 || ck == nil {
		t.Fatalf("%d", rec.Code)
	}
	if cookieNamed(rec, session.PendingName) != nil {
		t.Error("pending cookie set without a return_to")
	}
	if rec := ts.do("GET", "/account", "", ck); strings.Contains(rec.Body.String(), "Back to") {
		t.Errorf("account page shows a return_to link without one: %s", rec.Body.String())
	}
}

func TestTrialRejectsBadTokens(t *testing.T) {
	ts := newTestServer(t)
	expiredNonce, err := apihandoff.NewNonce()
	if err != nil {
		t.Fatal(err)
	}
	tokens := []string{"", "garbage", ts.handoff(apihandoff.PurposeLogin, "ann@example.com"),
		apihandoff.Mint([]byte("other"), apihandoff.Claims{Email: "a@b.c", Purpose: apihandoff.PurposeTrial, Nonce: "n", Expires: ts.now.Add(time.Minute)}),
		apihandoff.Mint(ts.TrialSecret, apihandoff.Claims{Email: "a@b.c", Purpose: apihandoff.PurposeTrial, Nonce: expiredNonce, Expires: ts.now.Add(-time.Minute)})}
	for _, tok := range tokens {
		rec := ts.do("GET", "/trial?t="+tok, "")
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "try again from the game site") || cookieNamed(rec, session.CookieName) != nil {
			t.Errorf("GET %q: %d", tok, rec.Code)
		}
		rec = ts.do("POST", "/trial", "t="+tok)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "try again from the game site") || cookieNamed(rec, session.CookieName) != nil {
			t.Errorf("POST %q: %d", tok, rec.Code)
		}
	}
	if n, _ := ts.store.ListAccounts(context.Background()); len(n) != 0 {
		t.Error("an account was created for a bad token")
	}
}

func TestTrialRollsBackOnEntitlementFailure(t *testing.T) {
	ts := newTestServer(t)
	ts.store.entitlementErr = errors.New("boom")
	tok := ts.handoff(apihandoff.PurposeTrial, "dana@example.com")
	rec := ts.do("POST", "/trial", "t="+tok)
	if rec.Code != 500 {
		t.Fatalf("first: %d", rec.Code)
	}
	a, err := ts.store.GetAccountByEmail(context.Background(), "dana@example.com")
	if err != nil {
		t.Fatal("no account")
	}
	if ents, _ := ts.store.ListEntitlements(context.Background(), a.ID); len(ents) != 0 {
		t.Errorf("entitlement leaked: %+v", ents)
	}

	ts.store.entitlementErr = nil
	tok2 := ts.handoff(apihandoff.PurposeTrial, "dana@example.com")
	rec = ts.do("POST", "/trial", "t="+tok2)
	if rec.Code != 302 || rec.Header().Get("Location") != "/account?notice=trial" {
		t.Errorf("retry: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestPatreonSessionHandoff(t *testing.T) {
	ts := newTestServer(t)
	tok := ts.handoff(apihandoff.PurposeLogin, "bob@example.com")
	rec := ts.do("GET", "/session?t="+tok, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "bob@example.com") || !strings.Contains(rec.Body.String(), `value="`+tok+`"`) ||
		!strings.Contains(rec.Body.String(), "Ann Example") {
		t.Fatalf("confirm: %d %s", rec.Code, rec.Body.String())
	}
	rec = ts.do("POST", "/session", "t="+tok)
	if rec.Code != 302 || rec.Header().Get("Location") != "/account?notice=login" || cookieNamed(rec, session.CookieName) == nil {
		t.Errorf("%d %q", rec.Code, rec.Header().Get("Location"))
	}

	pending := cookieFor(ts, map[string][]string{"package": {"all_data"}, "interval": {"monthly"}, "games": {"magic"}})
	rec = ts.do("POST", "/session", "t="+ts.handoff(apihandoff.PurposeLogin, "bob@example.com")+"&return_to=https%3A%2F%2Fpokemon.mtgban.com%2Fapi-plans", pending)
	if rec.Header().Get("Location") != "/checkout" {
		t.Errorf("pending: %q", rec.Header().Get("Location"))
	}
	merged := cookieNamed(rec, session.PendingName)
	pv, _ := ts.Sessions.Open(merged.Value)
	if pv.Get("package") != "all_data" || pv.Get("return_to") != "https://pokemon.mtgban.com/api-plans" {
		t.Errorf("pending after session merge: %v", pv)
	}

	if rec := ts.do("GET", "/session?t="+ts.handoff(apihandoff.PurposeTrial, "bob@example.com"), ""); rec.Code != 400 {
		t.Errorf("trial token on /session: %d", rec.Code)
	}

	tok3 := ts.handoff(apihandoff.PurposeLogin, "eve@example.com")
	if rec := ts.doCrossSite("POST", "/session", "t="+tok3); rec.Code != 403 {
		t.Errorf("cross-site session: %d", rec.Code)
	}
	rec = ts.do("POST", "/session", "t="+tok3)
	if rec.Code != 302 || cookieNamed(rec, session.CookieName) == nil {
		t.Errorf("nonce still usable after cross-site refusal: %d", rec.Code)
	}
}

func TestPatreonSessionWithoutReturnToSetsNoPendingCookie(t *testing.T) {
	ts := newTestServer(t)
	tok := ts.handoff(apihandoff.PurposeLogin, "noreturn2@example.com")
	rec := ts.do("POST", "/session", "t="+tok)
	ck := cookieNamed(rec, session.CookieName)
	if rec.Code != 302 || ck == nil {
		t.Fatalf("%d", rec.Code)
	}
	if cookieNamed(rec, session.PendingName) != nil {
		t.Error("pending cookie set without a return_to")
	}
	if rec := ts.do("GET", "/account", "", ck); strings.Contains(rec.Body.String(), "Back to") {
		t.Errorf("account page shows a return_to link without one: %s", rec.Body.String())
	}
}

func TestTrialConfirmGreetsEmailAloneWithoutName(t *testing.T) {
	ts := newTestServer(t)
	nonce, err := apihandoff.NewNonce()
	if err != nil {
		t.Fatal(err)
	}
	tok := apihandoff.Mint(ts.TrialSecret, apihandoff.Claims{Email: "noname@example.com", Purpose: apihandoff.PurposeTrial, Nonce: nonce, Expires: ts.now.Add(apihandoff.TTL)})
	rec := ts.do("GET", "/trial?t="+tok, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "noname@example.com") || strings.Contains(rec.Body.String(), "()") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestHandoffTokenIsSingleUse(t *testing.T) {
	ts := newTestServer(t)
	tok := ts.handoff(apihandoff.PurposeLogin, "carl@example.com")
	rec := ts.do("POST", "/session", "t="+tok)
	if rec.Code != 302 {
		t.Fatalf("first: %d", rec.Code)
	}
	rec = ts.do("POST", "/session", "t="+tok)
	if rec.Code != 400 {
		t.Fatalf("second: %d", rec.Code)
	}
}
