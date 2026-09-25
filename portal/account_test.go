package portal

import (
	"context"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/api-gatewahy/billing"
	"github.com/stripe/stripe-go/v84"
)

var keyRe = regexp.MustCompile(`ban_(?:live|demo)_[a-z0-9]{32}`)

func TestAccountPageAndKeys(t *testing.T) {
	ts := newTestServer(t)
	a, ck, csrf := ts.signIn(t, "ann@example.com")
	ctx := context.Background()
	_, _ = ts.store.AddEntitlement(ctx, apiaccess.Entitlement{AccountID: a.ID, Source: "manual", Games: []string{"magic"}, StoreScope: "ALL_ACCESS", Modes: []string{"retail", "buylist", "sealed"}})
	ts.store.usage = []memUsage{
		{Ts: ts.now, Row: apiaccess.UsageRow{AccountID: a.ID, Game: "magic", Requests: 42, Bytes: 4096}},
		{Ts: ts.now.AddDate(0, -1, 0), Row: apiaccess.UsageRow{AccountID: a.ID, Game: "magic", Requests: 77, Bytes: 1234}},
	}

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
	if len(ts.store.notified) == 0 {
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
	current := billing.Plan{Package: "starter", Interval: "monthly", Games: []string{"magic"}, Stores: []string{"CK"}}
	f.sub = &stripe.Subscription{ID: "sub_1", Metadata: current.Metadata(a.ID), Customer: &stripe.Customer{ID: "cus_test"},
		Items: &stripe.SubscriptionItemList{Data: []*stripe.SubscriptionItem{{ID: "si_1", Quantity: 1, Price: &stripe.Price{ID: "price_starter_monthly", LookupKey: "starter_monthly"}}}}}

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
	if u.Query().Get("change") != "1" || u.Query().Get("package") != "starter" || u.Query().Get("stores") != "CK" || u.Query().Get("games") != "magic" {
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
	if rec.Code != 303 || rec.Header().Get("Location") != "https://billing.stripe.com/p/session/test" {
		t.Errorf("portal: %d %q", rec.Code, rec.Header().Get("Location"))
	}

	rec = ts.do("GET", "/checkout?change=1&package=starter&games=magic&stores=CK,SCG&return_to=https%3A%2F%2Fmtgban.com%2Fapi-plans", "", ck)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `action="/account/plan"`) || !strings.Contains(rec.Body.String(), "$350") {
		t.Fatalf("change confirm: %d %s", rec.Code, rec.Body.String())
	}
	reconciled := ""
	ts.Reconcile = func(_ context.Context, id string) error { reconciled = id; return nil }
	rec = ts.do("POST", "/account/plan", "csrf="+csrf+"&package=starter&interval=monthly&games=magic&stores=CK,SCG", ck)
	if rec.Code != 302 || rec.Header().Get("Location") != "/account?notice=plan" || reconciled != "sub_1" || f.updated == nil {
		t.Errorf("change: %d %q reconciled %q updated %v", rec.Code, rec.Header().Get("Location"), reconciled, f.updated != nil)
	}

	rec = ts.do("POST", "/account/plan", "csrf="+csrf+"&package=starter&interval=monthly&games=chess&stores=CK", ck)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "chess") {
		t.Errorf("unknown game: %d %s", rec.Code, rec.Body.String())
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
