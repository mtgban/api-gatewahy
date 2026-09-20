package portal

import (
	"context"
	"errors"
	"strings"
	"testing"

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
	ts.store.SetStripeCustomerID(ctx, cust.ID, "cus_42")
	_, key, _ := ts.store.CreateKey(ctx, cust.ID, "old")
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
