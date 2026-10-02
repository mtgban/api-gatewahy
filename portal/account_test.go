package portal

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/api-gatewahy/billing"
	"github.com/mtgban/api-gatewahy/billing/billingtest"
	"github.com/stripe/stripe-go/v84"
)

var keyRe = regexp.MustCompile(`ban_(?:live|demo)_[a-z0-9]{32}`)

func TestAccountPageAndKeys(t *testing.T) {
	ts := newTestServer(t)
	a, ck, csrf := ts.signIn(t, "ann@example.com")
	ctx := context.Background()
	_, _ = ts.store.AddEntitlement(ctx, apiaccess.Entitlement{AccountID: a.ID, Source: "manual", Games: []string{"magic"}, StoreScope: "ALL_ACCESS", Modes: []string{"retail", "buylist", "sealed"}})
	ts.addUsage(42, apiaccess.Usage{Ts: ts.now, AccountID: a.ID, Game: "magic", Path: "/sets.json", Status: 200, Bytes: 100})
	ts.addUsage(77, apiaccess.Usage{Ts: ts.now.AddDate(0, -1, 0), AccountID: a.ID, Game: "magic", Path: "/sets.json", Status: 200, Bytes: 10})

	rec := ts.do("GET", "/account?notice=login", "", ck)
	body := rec.Body.String()
	for _, want := range []string{"ann@example.com", "Arranged with MTGBAN", "every store", "You are signed in.", "42", "No keys yet"} {
		if !strings.Contains(body, want) {
			t.Errorf("account page lacks %q", want)
		}
	}
	if strings.Contains(body, "77") {
		t.Error("last month's usage row shown in this month's table")
	}
	if strings.Contains(body, `href="/portal"`) {
		t.Error("portal button shown without a Stripe customer")
	}

	rec = ts.do("POST", "/account/keys", "csrf="+csrf+"&label=laptop", ck)
	body = rec.Body.String()
	plain := keyRe.FindString(body)
	if rec.Code != 200 || plain == "" || !strings.Contains(body, "laptop") || !strings.Contains(body, "shown once") {
		t.Fatalf("create: %d %s", rec.Code, body)
	}
	if !strings.Contains(ts.mail.String(), "A new API key") {
		t.Error("no key-created mail")
	}
	sep := strings.LastIndex(plain, "_")
	rec = ts.do("GET", "/account", "", ck)
	if strings.Contains(rec.Body.String(), plain) || !strings.Contains(rec.Body.String(), plain[sep+1:sep+9]) {
		t.Error("plaintext shown again, or prefix missing")
	}
	keys, _ := ts.store.ListKeys(ctx, a.ID)
	rec = ts.do("POST", "/account/keys/"+itoa(keys[0].ID)+"/revoke", "csrf="+csrf, ck)
	if rec.Code != 302 || rec.Header().Get("Location") != "/account?notice=revoked" {
		t.Fatalf("revoke: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if keys, _ = ts.store.ListKeys(ctx, a.ID); keys[0].RevokedAt == nil {
		t.Error("key not revoked")
	}
	if len(ts.store.Notified) == 0 {
		t.Error("no cache notify after revoke")
	}

	// Someone else's key cannot be revoked through this account.
	b, _ := ts.store.GetOrCreateAccount(ctx, "bob@example.com", "")
	_, bk, _ := ts.store.CreateKey(ctx, b.ID, "", apiaccess.KeyLive)
	if rec := ts.do("POST", "/account/keys/"+itoa(bk.ID)+"/revoke", "csrf="+csrf, ck); rec.Code != 404 {
		t.Errorf("cross-account revoke: %d", rec.Code)
	}
}

func TestPortalAndPlanChange(t *testing.T) {
	ts := newTestServer(t)
	f := ts.withStripe()
	a, ck, csrf := ts.signIn(t, "ann@example.com")
	ctx := context.Background()
	_, _ = ts.store.SetStripeCustomerID(ctx, a.ID, "cus_test")
	_, _ = ts.store.AddEntitlement(ctx, apiaccess.Entitlement{AccountID: a.ID, Source: "stripe", Games: []string{"magic"}, StoreScope: "TCGLow,TCGMarket,TCGDirect,TCGDirectNet,TCGPlayer,CK", Modes: []string{"retail", "buylist"}, Status: "active", ExternalRef: "sub_1"})
	current := billing.Plan{Package: "starter", Interval: "monthly", Games: []string{"magic"}, Stores: []string{"cardkingdom"}}
	f.AddSub(t, "sub_1", "cus_test", stripe.SubscriptionStatusActive, current.Metadata(a.ID), ts.now.AddDate(0, 1, 0), billingtest.Item{Key: "starter_monthly", Qty: 1})

	rec := ts.do("GET", "/account", "", ck)
	body := rec.Body.String()
	if !strings.Contains(body, `href="/portal"`) {
		t.Error("portal button missing")
	}
	changeURL := regexp.MustCompile(`href="([^"]+)">Change plan</a>`).FindStringSubmatch(body)
	if changeURL == nil {
		t.Fatalf("no change link in %s", body)
	}
	u, _ := url.Parse(strings.ReplaceAll(changeURL[1], "&amp;", "&"))
	if u.Query().Get("change") != "1" || u.Query().Get("package") != "starter" || u.Query().Get("stores") != "cardkingdom" || u.Query().Get("games") != "magic" {
		t.Errorf("change link %q", changeURL[1])
	}

	ts.PricingURL = "https://mtgban.com/api-plans?utm=x"
	rec = ts.do("GET", "/account", "", ck)
	changeURLWithUTM := regexp.MustCompile(`href="([^"]+)">Change plan</a>`).FindStringSubmatch(rec.Body.String())
	if changeURLWithUTM == nil {
		t.Fatalf("no change link with utm in %s", rec.Body.String())
	}
	u2, _ := url.Parse(strings.ReplaceAll(changeURLWithUTM[1], "&amp;", "&"))
	if u2.Query().Get("utm") != "x" || u2.Query().Get("change") != "1" {
		t.Errorf("change link lost pricing query %q", changeURLWithUTM[1])
	}
	ts.PricingURL = "https://mtgban.com/api-plans"

	rec = ts.do("GET", "/portal", "", ck)
	if rec.Code != 303 || rec.Header().Get("Location") != "https://billing.stripe.test/cus_test" {
		t.Errorf("portal: %d %q", rec.Code, rec.Header().Get("Location"))
	}

	rec = ts.do("GET", "/checkout?change=1&package=starter&games=magic&stores=cardkingdom,starcitygames&return_to=https%3A%2F%2Fmtgban.com%2Fapi-plans", "", ck)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `action="/account/plan"`) || !strings.Contains(rec.Body.String(), "$350") {
		t.Fatalf("change confirm: %d %s", rec.Code, rec.Body.String())
	}
	reconciled := ""
	ts.Reconcile = func(_ context.Context, id string) error { reconciled = id; return nil }
	rec = ts.do("POST", "/account/plan", "csrf="+csrf+"&package=starter&interval=monthly&games=magic&stores=cardkingdom,starcitygames", ck)
	if rec.Code != 302 || rec.Header().Get("Location") != "/account?notice=plan" || reconciled != "sub_1" || len(f.Updates["sub_1"]) != 1 {
		t.Errorf("change: %d %q reconciled %q updates %d", rec.Code, rec.Header().Get("Location"), reconciled, len(f.Updates["sub_1"]))
	}

	rec = ts.do("POST", "/account/plan", "csrf="+csrf+"&package=starter&interval=monthly&games=chess&stores=cardkingdom", ck)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "chess") {
		t.Errorf("unknown game: %d %s", rec.Code, rec.Body.String())
	}
}

// TestChangePlanLogsEntitlementListFailure covers item 24: changePlan must
// log a store failure instead of dropping it.
func TestChangePlanLogsEntitlementListFailure(t *testing.T) {
	ts := newTestServer(t)
	ts.withStripe()
	_, ck, csrf := ts.signIn(t, "ann@example.com")
	ts.store.Fail["ListEntitlements"] = errors.New("entitlements down")
	rec := ts.do("POST", "/account/plan", "csrf="+csrf+"&package=all_data&interval=monthly&games=magic", ck)
	if rec.Code != 500 {
		t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(ts.logBuf.String(), "entitlements down") {
		t.Errorf("entitlements failure not logged: %q", ts.logBuf.String())
	}
}

func TestPortalWithoutCustomer(t *testing.T) {
	ts := newTestServer(t)
	ts.withStripe()
	_, ck, _ := ts.signIn(t, "ann@example.com")
	rec := ts.do("GET", "/portal", "", ck)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "no billing") {
		t.Errorf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestCreateKeyCapsLabelByRunes(t *testing.T) {
	ts := newTestServer(t)
	a, ck, csrf := ts.signIn(t, "runes@example.com")
	label := strings.Repeat("é", 70)
	rec := ts.do("POST", "/account/keys", "csrf="+csrf+"&label="+url.QueryEscape(label), ck)
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	keys, _ := ts.store.ListKeys(context.Background(), a.ID)
	if len(keys) != 1 {
		t.Fatalf("keys: %d", len(keys))
	}
	got := keys[0].Label
	if n := utf8.RuneCountInString(got); n != 64 {
		t.Errorf("label runes: got %d want 64", n)
	}
	if !utf8.ValidString(got) {
		t.Errorf("label is not valid utf8: %q", got)
	}
}

func TestCreateKeyRateLimit(t *testing.T) {
	ts := newTestServer(t)
	a, ck, csrf := ts.signIn(t, "many@example.com")
	for i := 0; i < 11; i++ {
		rec := ts.do("POST", "/account/keys", "csrf="+csrf+"&label=k"+strconv.Itoa(i), ck)
		want := 200
		if i == 10 {
			want = 429
		}
		if rec.Code != want {
			t.Errorf("key %d: got %d want %d", i, rec.Code, want)
		}
		if i == 10 && !strings.Contains(rec.Body.String(), "Too many keys") {
			t.Errorf("eleventh body: %s", rec.Body.String())
		}
		if rec.Code == 200 {
			// Revoke right away so the active-key cap never interferes with this rate-limit test.
			keys, _ := ts.store.ListKeys(context.Background(), a.ID)
			_, _ = ts.store.RevokeKey(context.Background(), keys[len(keys)-1].ID, a.ID)
		}
	}
}

func TestKeyNeedsALabelAndAccountsHoldFive(t *testing.T) {
	ts := newTestServer(t)
	_, ck, csrf := ts.signIn(t, "ann@example.com")
	rec := ts.do("POST", "/account/keys", "csrf="+csrf+"&label=", ck)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "Give the key a label") {
		t.Fatalf("empty label: %d %s", rec.Code, rec.Body.String())
	}
	for i := 0; i < maxActiveKeys; i++ {
		if rec := ts.do("POST", "/account/keys", "csrf="+csrf+"&label=k"+strconv.Itoa(i), ck); rec.Code != 200 {
			t.Fatalf("key %d: %d", i, rec.Code)
		}
	}
	rec = ts.do("POST", "/account/keys", "csrf="+csrf+"&label=one-too-many", ck)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "Revoke one you no longer use") {
		t.Fatalf("sixth key: %d %s", rec.Code, rec.Body.String())
	}
}

func TestKeyKindFollowsTheAccountsAccess(t *testing.T) {
	ts := newTestServer(t)
	a, ck, csrf := ts.signIn(t, "ann@example.com")
	rec := ts.do("POST", "/account/keys", "csrf="+csrf+"&label=demo", ck)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "ban_demo_") {
		t.Fatalf("no plan: %d, body lacks a demo key", rec.Code)
	}
	_, _ = ts.store.AddEntitlement(context.Background(), entitlementFor(a.ID, "stripe", "BASE_ACCESS"))
	rec = ts.do("POST", "/account/keys", "csrf="+csrf+"&label=paid", ck)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "ban_live_") {
		t.Fatalf("paid plan: %d, body lacks a live key", rec.Code)
	}
}

func TestNewKeyPageExplainsUse(t *testing.T) {
	ts := newTestServer(t)
	_, ck, csrf := ts.signIn(t, "ann@example.com")
	rec := ts.do("POST", "/account/keys", "csrf="+csrf+"&label=laptop", ck)
	body := rec.Body.String()
	for _, want := range []string{"Using your key", "curl -H", "https://api.test/v1/magic/mtgban/retail/ZEN.json", `href="https://mtgban.com/guide#api-getting-started"`} {
		if !strings.Contains(body, want) {
			t.Errorf("new key page lacks %q", want)
		}
	}
}

func TestAccountPageShowsKeyKind(t *testing.T) {
	ts := newTestServer(t)
	a, ck, _ := ts.signIn(t, "ann@example.com")
	ctx := context.Background()
	if _, _, err := ts.store.CreateKey(ctx, a.ID, "laptop", apiaccess.KeyDemo); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ts.store.CreateKey(ctx, a.ID, "server", apiaccess.KeyLive); err != nil {
		t.Fatal(err)
	}
	body := ts.do("GET", "/account", "", ck).Body.String()
	for _, want := range []string{"ban_demo", "ban_live"} {
		if !strings.Contains(body, want) {
			t.Errorf("account page lacks the key kind %q", want)
		}
	}
}

// TestAccountPageShowsTryAgainOnListFailure covers item 16: a store failure
// must show tryAgainMsg at 500, not empty lists that read as no access.
func TestAccountPageShowsTryAgainOnListFailure(t *testing.T) {
	ts := newTestServer(t)
	_, ck, _ := ts.signIn(t, "ann@example.com")

	ts.store.Fail["ListEntitlements"] = errors.New("entitlements down")
	rec := ts.do("GET", "/account", "", ck)
	body := rec.Body.String()
	if rec.Code != 500 || !strings.Contains(body, tryAgainMsg) {
		t.Errorf("entitlements error not surfaced: %d %s", rec.Code, body)
	}
	if strings.Contains(body, "No active access") {
		t.Error("entitlements failure still shows the empty-state plan pitch")
	}

	delete(ts.store.Fail, "ListEntitlements")
	ts.store.Fail["ListKeys"] = errors.New("keys down")
	rec = ts.do("GET", "/account", "", ck)
	body = rec.Body.String()
	if rec.Code != 500 || !strings.Contains(body, tryAgainMsg) {
		t.Errorf("keys error not surfaced: %d %s", rec.Code, body)
	}
	if strings.Contains(body, "No keys yet") {
		t.Error("keys failure still shows the empty-state line")
	}
}

// TestAccountPageListFailureDoesNotClobberFormError covers item 24's
// no-clobber guard: a form error set before the list calls run must survive
// a list failure, not be overwritten by the generic tryAgainMsg.
func TestAccountPageListFailureDoesNotClobberFormError(t *testing.T) {
	ts := newTestServer(t)
	_, ck, csrf := ts.signIn(t, "ann@example.com")
	ts.store.Fail["ListEntitlements"] = errors.New("entitlements down")
	rec := ts.do("POST", "/account/keys", "csrf="+csrf, ck)
	body := rec.Body.String()
	if rec.Code != 400 || !strings.Contains(body, "Give the key a label") || strings.Contains(body, tryAgainMsg) {
		t.Errorf("form error clobbered by list error: %d %s", rec.Code, body)
	}
}

// TestCreateKeyKeepsNewKeyVisibleWhenListsFail covers item 16's follow-up: a
// list failure right after minting a key must not invite a retry that burns
// a second key.
func TestCreateKeyKeepsNewKeyVisibleWhenListsFail(t *testing.T) {
	ts := newTestServer(t)
	_, ck, csrf := ts.signIn(t, "ann@example.com")
	// The plan-kind check also lists entitlements; let that one succeed and
	// only fail the second call, the one inside the final re-render.
	ts.store.Fail["ListEntitlements"] = errors.New("entitlements down")
	ts.store.FailFrom["ListEntitlements"] = ts.store.Calls["ListEntitlements"] + 2
	rec := ts.do("POST", "/account/keys", "csrf="+csrf+"&label=laptop", ck)
	body := rec.Body.String()
	plain := keyRe.FindString(body)
	if rec.Code != 200 || plain == "" {
		t.Fatalf("key creation should still succeed: %d %s", rec.Code, body)
	}
	if strings.Contains(body, tryAgainMsg) {
		t.Error("new key shown next to try-again, inviting a second key")
	}
	if !strings.Contains(body, "reload") {
		t.Errorf("no reload guidance next to the new key: %s", body)
	}
}

func TestRejectedKeyCreationKeepsTheQuota(t *testing.T) {
	ts := newTestServer(t)
	_, ck, csrf := ts.signIn(t, "many@example.com")
	for i := 0; i < keysPerHour; i++ {
		if rec := ts.do("POST", "/account/keys", "csrf="+csrf+"&label=", ck); rec.Code != 400 {
			t.Fatalf("empty label %d: %d", i, rec.Code)
		}
	}
	rec := ts.do("POST", "/account/keys", "csrf="+csrf+"&label=laptop", ck)
	if rec.Code != 200 {
		t.Fatalf("a rejected request spent the hourly quota: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPrefillMapsShorthandsBackToKeys(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	plan := billing.Plan{Package: "starter", Interval: "monthly", Games: []string{"magic", "pokemon"}, Stores: []string{"cardkingdom", "starcitygames"}}
	resolved, err := plan.Resolve(ctx, ts.Catalog, ts.Stores)
	if err != nil {
		t.Fatal(err)
	}
	e := apiaccess.Entitlement{Source: "stripe", Games: plan.Games, StoreScope: resolved.Scope + ",GoneStore"}
	q := ts.prefillQuery(ts.newSiteLookup(ctx), e)
	if q.Get("package") != "starter" || q.Get("stores") != "cardkingdom,starcitygames" || q.Get("games") != "magic,pokemon" || q.Get("change") != "1" {
		t.Errorf("prefill %v", q)
	}
	if q := ts.prefillQuery(ts.newSiteLookup(ctx), apiaccess.Entitlement{Games: []string{"magic"}, StoreScope: "ALL_ACCESS"}); q.Get("package") != "all_data" || q.Has("stores") {
		t.Errorf("preset prefill %v", q)
	}
}

func TestPrefillKeepsTheStoresOutWhenASiteIsDown(t *testing.T) {
	ts := newTestServer(t)
	ts.stores.Down = map[string]bool{"pokemon": true}
	e := apiaccess.Entitlement{Source: "stripe", Games: []string{"magic", "pokemon"}, StoreScope: "CK,SCG,TCGLow,TNT"}
	if q := ts.prefillQuery(ts.newSiteLookup(context.Background()), e); q.Has("stores") || q.Get("package") != "starter" {
		t.Errorf("prefill with pokemon down %v", q)
	}
}

func TestAccountPageReadsEachSiteOnce(t *testing.T) {
	ts := newTestServer(t)
	a, ck, _ := ts.signIn(t, "ann@example.com")
	ctx := context.Background()
	for _, ref := range []string{"sub_1", "sub_2"} {
		_, _ = ts.store.AddEntitlement(ctx, apiaccess.Entitlement{AccountID: a.ID, Source: "stripe", Games: []string{"magic"}, StoreScope: "CK,TCGLow", Modes: []string{"retail"}, Status: "active", ExternalRef: ref})
	}
	rec := ts.do("GET", "/account", "", ck)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Card Kingdom") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if n := ts.stores.Calls["magic"]; n != 1 {
		t.Errorf("magic read %d times for one render", n)
	}
}

func TestPlanChangeRejectsAStoreTheSitesDoNotSell(t *testing.T) {
	ts := newTestServer(t)
	f := ts.withStripe()
	a, ck, csrf := ts.signIn(t, "ann@example.com")
	ctx := context.Background()
	_, _ = ts.store.SetStripeCustomerID(ctx, a.ID, "cus_test")
	_, _ = ts.store.AddEntitlement(ctx, entitlementFor(a.ID, "stripe", "BASE_ACCESS"))
	f.AddSub(t, "sub_1", "cus_test", stripe.SubscriptionStatusActive,
		billing.Plan{Package: "all_stores", Interval: "monthly", Games: []string{"magic"}}.Metadata(a.ID), ts.now.AddDate(0, 1, 0))
	rec := ts.do("POST", "/account/plan", "csrf="+csrf+"&package=starter&interval=monthly&games=magic&stores=trollandtoad", ck)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "Store trollandtoad is not available for the games you picked.") || len(f.Updates["sub_1"]) != 0 {
		t.Errorf("%d %d %s", rec.Code, len(f.Updates["sub_1"]), rec.Body.String())
	}
}

func TestPlanChangeDeclinedCardKeepsThePlan(t *testing.T) {
	ts := newTestServer(t)
	f := ts.withStripe()
	a, ck, csrf := ts.signIn(t, "ann@example.com")
	ctx := context.Background()
	_, _ = ts.store.SetStripeCustomerID(ctx, a.ID, "cus_test")
	_, _ = ts.store.AddEntitlement(ctx, entitlementFor(a.ID, "stripe", "BASE_ACCESS"))
	f.AddSub(t, "sub_1", "cus_test", stripe.SubscriptionStatusActive,
		billing.Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}.Metadata(a.ID), ts.now.AddDate(0, 1, 0))
	f.Fail["UpdateSubscription"] = &stripe.Error{HTTPStatusCode: 402, Code: stripe.ErrorCodeCardDeclined, Msg: "Your card was declined."}
	reconciled := false
	ts.Reconcile = func(context.Context, string) error { reconciled = true; return nil }

	rec := ts.do("POST", "/account/plan", "csrf="+csrf+"&package=all_stores&interval=monthly&games=magic", ck)
	body := rec.Body.String()
	if rec.Code != 402 || !strings.Contains(body, "Your card was declined. Update it under Manage billing, then try again.") || strings.Contains(body, "Could not") {
		t.Errorf("%d %s", rec.Code, body)
	}
	if reconciled || len(f.Updates["sub_1"]) != 0 {
		t.Errorf("declined upgrade applied: reconciled %v updates %d", reconciled, len(f.Updates["sub_1"]))
	}
}

func TestPlanChangeThatStripeTookButReconcileMissed(t *testing.T) {
	ts := newTestServer(t)
	f := ts.withStripe()
	a, ck, csrf := ts.signIn(t, "ann@example.com")
	ctx := context.Background()
	_, _ = ts.store.SetStripeCustomerID(ctx, a.ID, "cus_test")
	_, _ = ts.store.AddEntitlement(ctx, entitlementFor(a.ID, "stripe", "BASE_ACCESS"))
	f.AddSub(t, "sub_1", "cus_test", stripe.SubscriptionStatusActive,
		billing.Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}.Metadata(a.ID), ts.now.AddDate(0, 1, 0))
	ts.Reconcile = func(context.Context, string) error { return errors.New("db down") }

	rec := ts.do("POST", "/account/plan", "csrf="+csrf+"&package=all_stores&interval=monthly&games=magic", ck)
	if rec.Code != 302 || rec.Header().Get("Location") != "/account?notice=plan_pending" || len(f.Updates["sub_1"]) != 1 {
		t.Fatalf("%d %q updates %d: %s", rec.Code, rec.Header().Get("Location"), len(f.Updates["sub_1"]), rec.Body.String())
	}
	rec = ts.do("GET", "/account?notice=plan_pending", "", ck)
	if body := rec.Body.String(); !strings.Contains(body, "Plan changed; your access updates shortly.") || strings.Contains(body, "Could not start checkout") {
		t.Errorf("account page: %s", body)
	}
}
