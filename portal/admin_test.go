package portal

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/api-gatewahy/billing"
)

func TestAdminIsHiddenFromNonAdmins(t *testing.T) {
	ts := newTestServer(t)
	_, ck, _ := ts.signIn(t, "ann@example.com")
	for _, path := range []string{"/admin", "/admin/usage", "/admin/accounts/1"} {
		if rec := ts.do("GET", path, "", ck); rec.Code != 404 {
			t.Errorf("%s: %d", path, rec.Code)
		}
	}
	if rec := ts.do("GET", "/admin", ""); rec.Code != 302 {
		t.Errorf("anonymous: %d", rec.Code)
	}
}

func TestAdminAccountsAndActions(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	_, ck, csrf := ts.signIn(t, "Admin@Example.com")
	cust, _ := ts.store.GetOrCreateAccount(ctx, "cust@example.com", "")
	_, _ = ts.store.SetStripeCustomerID(ctx, cust.ID, "cus_42")
	_, key, _ := ts.store.CreateKey(ctx, cust.ID, "old", apiaccess.KeyLive)
	id := itoa(cust.ID)

	rec := ts.do("GET", "/admin?q=cust", "", ck)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "cust@example.com") || strings.Contains(rec.Body.String(), "admin@example.com") {
		t.Fatalf("search: %d %s", rec.Code, rec.Body.String())
	}
	rec = ts.do("GET", "/admin/accounts/"+id, "", ck)
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, key.Prefix) || !strings.Contains(body, "https://dashboard.stripe.com/customers/cus_42") || !strings.Contains(body, `name="stores"`) {
		t.Fatalf("detail: %d %s", rec.Code, body)
	}

	rec = ts.do("POST", "/admin/accounts/"+id+"/status", "csrf="+csrf+"&status=suspended", ck)
	if a, _ := ts.store.GetAccount(ctx, cust.ID); rec.Code != 302 || a.Status != "suspended" {
		t.Errorf("suspend: %d %s", rec.Code, a.Status)
	}
	ts.do("POST", "/admin/accounts/"+id+"/status", "csrf="+csrf+"&status=active", ck)
	if rec := ts.do("POST", "/admin/accounts/"+id+"/status", "csrf="+csrf+"&status=deleted", ck); rec.Code != 400 {
		t.Errorf("bad status: %d", rec.Code)
	}
	ts.do("POST", "/admin/accounts/"+id+"/note", "csrf="+csrf+"&note=vip+customer", ck)
	if a, _ := ts.store.GetAccount(ctx, cust.ID); a.Note != "vip customer" {
		t.Errorf("note %q", a.Note)
	}

	rec = ts.do("POST", "/admin/accounts/"+id+"/keys/"+itoa(key.ID)+"/revoke", "csrf="+csrf, ck)
	if keys, _ := ts.store.ListKeys(ctx, cust.ID); rec.Code != 302 || keys[0].RevokedAt == nil {
		t.Error("key not revoked")
	}

	form := "csrf=" + csrf + "&games=magic&games=pokemon&stores=BASE_ACCESS&modes=retail&modes=buylist&until=2027-01-31&note=arranged"
	rec = ts.do("POST", "/admin/accounts/"+id+"/entitlements", form, ck)
	ents, _ := ts.store.ListEntitlements(ctx, cust.ID)
	if rec.Code != 302 || len(ents) != 1 || ents[0].Source != "manual" || ents[0].StoreScope != "BASE_ACCESS" || ents[0].ValidUntil == nil || ents[0].Note != "arranged" {
		t.Fatalf("grant: %d %+v", rec.Code, ents)
	}
	if rec := ts.do("POST", "/admin/accounts/"+id+"/entitlements", "csrf="+csrf+"&games=chess&stores=ALL_ACCESS&modes=retail", ck); rec.Code != 400 || !strings.Contains(rec.Body.String(), "chess") {
		t.Errorf("bad game: %d", rec.Code)
	}
	if rec := ts.do("POST", "/admin/accounts/"+id+"/entitlements", "csrf="+csrf+"&games=magic&stores=ALL_ACCESS&modes=fast", ck); rec.Code != 400 {
		t.Errorf("bad mode: %d", rec.Code)
	}
	rec = ts.do("POST", "/admin/accounts/"+id+"/entitlements/"+itoa(ents[0].ID)+"/end", "csrf="+csrf, ck)
	if ents, _ = ts.store.ListEntitlements(ctx, cust.ID); rec.Code != 302 || ents[0].Status != "ended" {
		t.Error("entitlement not ended")
	}

	rec = ts.do("POST", "/admin/accounts/"+id+"/invites", "csrf="+csrf+"&interval=quarterly&days=7&note=deal", ck)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "inv"+strings.Repeat("y", 30)) {
		t.Errorf("invite: %d %s", rec.Code, rec.Body.String())
	}
	if rec := ts.do("POST", "/admin/accounts/"+id+"/invites", "csrf="+csrf+"&interval=monthly", ck); rec.Code != 400 {
		t.Errorf("public interval invite: %d", rec.Code)
	}

	rec = ts.do("GET", "/admin/accounts/"+id, "", ck)
	body = rec.Body.String()
	if !strings.Contains(body, "Activity") {
		t.Fatalf("no activity table: %s", body)
	}
	// These assert on text only the audit rows can produce: the grant row's
	// games/scope/modes detail, the invite row's "<n> days" detail, and the
	// note row's action word in its own cell. "suspended" is not used here
	// since the status form also renders that word as a hidden field value.
	if !strings.Contains(body, "magic,pokemon BASE_ACCESS retail,buylist") {
		t.Errorf("activity log missing grant detail: %s", body)
	}
	if !strings.Contains(body, "7 days") {
		t.Errorf("activity log missing invite detail: %s", body)
	}
	if !strings.Contains(body, "<td>note</td>") {
		t.Errorf("activity log missing note action: %s", body)
	}
	if !strings.Contains(body, "admin@example.com") {
		t.Errorf("activity log missing actor: %s", body)
	}
	if !regexp.MustCompile(`<td>\d{4}-\d{2}-\d{2} \d{2}:\d{2}</td>`).MatchString(body) {
		t.Errorf("activity log missing time of day: %s", body)
	}

	ts.ReconcileAll = func(context.Context) (billing.Result, error) { return billing.Result{Checked: 1}, nil }
	ts.do("POST", "/admin/reconcile", "csrf="+csrf, ck)
	found := false
	for _, act := range ts.store.actions {
		if act.Action == "reconcile" && act.AccountID == 0 {
			found = true
		}
	}
	if !found {
		t.Error("reconcile did not record an admin action for account 0")
	}
}

func TestAdminAccountListErrorsAreLoggedAndDoNotClobber(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	_, ck, csrf := ts.signIn(t, "admin@example.com")
	cust, _ := ts.store.GetOrCreateAccount(ctx, "cust2@example.com", "")
	id := itoa(cust.ID)

	ts.store.listKeysErr = errors.New("keys down")
	rec := ts.do("GET", "/admin/accounts/"+id, "", ck)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), tryAgainMsg) {
		t.Errorf("keys error not surfaced: %d %s", rec.Code, rec.Body.String())
	}

	ts.store.listKeysErr = nil
	ts.store.listActionsErr = errors.New("actions down")
	rec = ts.do("GET", "/admin/accounts/"+id, "", ck)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), tryAgainMsg) {
		t.Errorf("actions error not surfaced: %d %s", rec.Code, rec.Body.String())
	}

	// A form error set before the list calls run must survive a list failure,
	// not be overwritten by the generic tryAgainMsg.
	ts.store.listEntitlementsErr = errors.New("entitlements down")
	rec = ts.do("POST", "/admin/accounts/"+id+"/status", "csrf="+csrf+"&status=bogus", ck)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "Status must be active or suspended.") {
		t.Errorf("form error clobbered by list error: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAdminAddEntitlementStoreFailureIs500(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	_, ck, csrf := ts.signIn(t, "admin@example.com")
	cust, _ := ts.store.GetOrCreateAccount(ctx, "cust3@example.com", "")
	id := itoa(cust.ID)

	ts.store.entitlementErr = errors.New("db down")
	form := "csrf=" + csrf + "&games=magic&stores=ALL_ACCESS&modes=retail"
	rec := ts.do("POST", "/admin/accounts/"+id+"/entitlements", form, ck)
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), tryAgainMsg) {
		t.Errorf("store failure: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAdminGrantRejectsPastUntil(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	_, ck, csrf := ts.signIn(t, "admin@example.com")
	cust, _ := ts.store.GetOrCreateAccount(ctx, "cust4@example.com", "")
	id := itoa(cust.ID)

	form := "csrf=" + csrf + "&games=magic&stores=ALL_ACCESS&modes=retail&until=2020-01-01"
	rec := ts.do("POST", "/admin/accounts/"+id+"/entitlements", form, ck)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "Until must be in the future.") {
		t.Errorf("past until: %d %s", rec.Code, rec.Body.String())
	}
	if ents, _ := ts.store.ListEntitlements(ctx, cust.ID); len(ents) != 0 {
		t.Error("entitlement with a past until was still added")
	}
}

func TestAdminInviteRejectsLongDays(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	_, ck, csrf := ts.signIn(t, "admin@example.com")
	cust, _ := ts.store.GetOrCreateAccount(ctx, "cust5@example.com", "")
	id := itoa(cust.ID)

	rec := ts.do("POST", "/admin/accounts/"+id+"/invites", "csrf="+csrf+"&interval=quarterly&days=400", ck)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "365") {
		t.Errorf("long invite: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAdminInviteRejectsBadDays(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	_, ck, csrf := ts.signIn(t, "admin@example.com")
	cust, _ := ts.store.GetOrCreateAccount(ctx, "cust6@example.com", "")
	id := itoa(cust.ID)

	for _, days := range []string{"0", "-5", "abc", ""} {
		rec := ts.do("POST", "/admin/accounts/"+id+"/invites", "csrf="+csrf+"&interval=quarterly&days="+days, ck)
		if rec.Code != 400 {
			t.Errorf("days=%q: %d", days, rec.Code)
		}
	}
}

func TestAdminHomeShowsRecentActivity(t *testing.T) {
	ts := newTestServer(t)
	_, ck, csrf := ts.signIn(t, "admin@example.com")
	ts.ReconcileAll = func(context.Context) (billing.Result, error) { return billing.Result{Checked: 1}, nil }
	ts.do("POST", "/admin/reconcile", "csrf="+csrf, ck)
	rec := ts.do("GET", "/admin", "", ck)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "<td>reconcile</td>") {
		t.Errorf("admin home missing reconcile activity: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAdminUsageMapsErrorsAndValidatesRange(t *testing.T) {
	ts := newTestServer(t)
	_, ck, _ := ts.signIn(t, "admin@example.com")

	if rec := ts.do("GET", "/admin/usage?since=2026-09-20&until=2026-09-01", "", ck); rec.Code != 400 {
		t.Errorf("since after until: %d", rec.Code)
	}
	ts.store.usageErr = apiaccess.ErrNotFound
	if rec := ts.do("GET", "/admin/usage", "", ck); rec.Code != 404 {
		t.Errorf("not found: %d", rec.Code)
	}
	ts.store.usageErr = errors.New("db down")
	if rec := ts.do("GET", "/admin/usage", "", ck); rec.Code != 500 {
		t.Errorf("other error: %d", rec.Code)
	}
}

func TestAdminRevokeKeyCrossAccountFails(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	_, ck, csrf := ts.signIn(t, "admin@example.com")
	a1, _ := ts.store.GetOrCreateAccount(ctx, "a1@example.com", "")
	a2, _ := ts.store.GetOrCreateAccount(ctx, "a2@example.com", "")
	_, key, _ := ts.store.CreateKey(ctx, a2.ID, "b-key", apiaccess.KeyLive)

	rec := ts.do("POST", "/admin/accounts/"+itoa(a1.ID)+"/keys/"+itoa(key.ID)+"/revoke", "csrf="+csrf, ck)
	if rec.Code != 404 {
		t.Errorf("cross-account revoke: %d", rec.Code)
	}
	keys, _ := ts.store.ListKeys(ctx, a2.ID)
	if len(keys) != 1 || keys[0].RevokedAt != nil {
		t.Error("key of account B was revoked via account A's route")
	}
}

func TestAdminUsageAndReconcile(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	_, ck, csrf := ts.signIn(t, "admin@example.com")
	cust, _ := ts.store.GetOrCreateAccount(ctx, "cust@example.com", "")
	ts.store.usage = []memUsage{
		{Ts: ts.now, Row: apiaccess.UsageRow{AccountID: cust.ID, Email: "cust@example.com", Game: "magic", Requests: 5}},
		{Ts: ts.now, Row: apiaccess.UsageRow{AccountID: cust.ID, Email: "cust@example.com", Game: "pokemon", Requests: 7}},
	}

	rec := ts.do("GET", "/admin/usage?since=2026-09-01&until=2026-10-01&game=pokemon", "", ck)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "pokemon") || strings.Contains(rec.Body.String(), ">magic<") {
		t.Errorf("usage filter: %d %s", rec.Code, rec.Body.String())
	}
	if rec := ts.do("GET", "/admin/usage?since=yesterday", "", ck); rec.Code != 400 {
		t.Errorf("bad date: %d", rec.Code)
	}

	if rec := ts.do("POST", "/admin/reconcile", "csrf="+csrf, ck); rec.Code != 503 {
		t.Errorf("reconcile without stripe: %d", rec.Code)
	}
	ts.ReconcileAll = func(context.Context) (billing.Result, error) { return billing.Result{Checked: 3}, nil }
	rec = ts.do("POST", "/admin/reconcile", "csrf="+csrf, ck)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), billing.Result{Checked: 3}.Summary()) {
		t.Errorf("reconcile: %d %s", rec.Code, rec.Body.String())
	}
	ts.ReconcileAll = func(context.Context) (billing.Result, error) { return billing.Result{}, errors.New("stripe down") }
	if rec := ts.do("POST", "/admin/reconcile", "csrf="+csrf, ck); rec.Code != 502 {
		t.Errorf("reconcile failure: %d", rec.Code)
	}
}

func TestAdminDestructiveButtonsConfirm(t *testing.T) {
	ts := newTestServer(t)
	_, ck, _ := ts.signIn(t, "admin@example.com")
	a, _ := ts.store.GetOrCreateAccount(context.Background(), "cust@example.com", "")
	_, _ = ts.store.AddEntitlement(context.Background(), entitlementFor(a.ID, "manual", "BASE_ACCESS"))
	_, _, _ = ts.store.CreateKey(context.Background(), a.ID, "k", apiaccess.KeyLive)
	body := ts.do("GET", "/admin/accounts/"+strconv.FormatInt(a.ID, 10), "", ck).Body.String()
	for _, action := range []string{"/status", "/revoke", "/end"} {
		i := strings.Index(body, `action="/admin/accounts/`+strconv.FormatInt(a.ID, 10))
		if i < 0 {
			t.Fatalf("no form for %s", action)
		}
	}
	if strings.Count(body, "onsubmit=\"return confirm(") < 3 {
		t.Errorf("expected a confirm on suspend, revoke, and end; got %d", strings.Count(body, "onsubmit=\"return confirm("))
	}
}

func TestInvitePanelExplainsTheSchedule(t *testing.T) {
	ts := newTestServer(t)
	_, ck, _ := ts.signIn(t, "admin@example.com")
	a, _ := ts.store.GetOrCreateAccount(context.Background(), "cust@example.com", "")
	body := ts.do("GET", "/admin/accounts/"+strconv.FormatInt(a.ID, 10), "", ck).Body.String()
	for _, want := range []string{"Invite to a billing schedule", "invite-only", "Billing schedule", "every 3 months"} {
		if !strings.Contains(body, want) {
			t.Errorf("invite panel lacks %q", want)
		}
	}
}

func TestAdminHomeListsDemoAccess(t *testing.T) {
	ts := newTestServer(t)
	_, ck, _ := ts.signIn(t, "admin@example.com")
	ctx := context.Background()
	a, _ := ts.store.GetOrCreateAccount(ctx, "trial@example.com", "")
	ends := ts.now.Add(15 * 24 * time.Hour)
	e := entitlementFor(a.ID, "trial", "ALL_ACCESS")
	e.ValidUntil = &ends
	_, _ = ts.store.AddEntitlement(ctx, e)
	_, _ = ts.store.CreateTrial(ctx, "patron@example.com", a.ID, ends, ts.now.Add(-time.Hour))
	_, _, _ = ts.store.CreateKey(ctx, a.ID, "k", apiaccess.KeyDemo)
	body := ts.do("GET", "/admin", "", ck).Body.String()
	for _, want := range []string{"Demo access", "trial@example.com", "patron@example.com", "<td>1</td>"} {
		if !strings.Contains(body, want) {
			t.Errorf("admin home lacks %q", want)
		}
	}
}

func TestAdminUsageByKeyAndPaths(t *testing.T) {
	ts := newTestServer(t)
	_, ck, _ := ts.signIn(t, "admin@example.com")
	ctx := context.Background()
	a, _ := ts.store.GetOrCreateAccount(ctx, "u@example.com", "")
	_, k, _ := ts.store.CreateKey(ctx, a.ID, "laptop", apiaccess.KeyLive)
	ts.store.addUsage(a.ID, k.ID, "magic", "/retail/ZEN.json", 200, ts.now.Add(-time.Hour))
	ts.store.addUsage(a.ID, k.ID, "magic", "/sets.json", 404, ts.now.Add(-time.Hour))
	since, until := ts.now.Add(-48*time.Hour).Format("2006-01-02"), ts.now.Add(24*time.Hour).Format("2006-01-02")
	body := ts.do("GET", "/admin/usage?since="+since+"&until="+until+"&key="+k.Prefix, "", ck).Body.String()
	for _, want := range []string{"By key", k.Prefix, "laptop", "/retail/ZEN.json", "/sets.json"} {
		if !strings.Contains(body, want) {
			t.Errorf("usage page lacks %q", want)
		}
	}
	body = ts.do("GET", "/admin/accounts/"+strconv.FormatInt(a.ID, 10), "", ck).Body.String()
	if !strings.Contains(body, "Usage this month") || !strings.Contains(body, k.Prefix) {
		t.Error("account page lacks the per-key usage")
	}
	if strings.Contains(body, "No requests this month.") {
		t.Error("account page reports no usage for a month with two requests")
	}
	// Only the usage table renders the day's two requests and one error.
	if !strings.Contains(body, "<td>2</td><td>1</td>") {
		t.Error("account page lacks the request and error counts")
	}
}
