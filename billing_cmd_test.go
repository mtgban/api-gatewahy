package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/api-gatewahy/apiaccess/apiaccesstest"
	"github.com/mtgban/api-gatewahy/billing"
	"github.com/mtgban/api-gatewahy/billing/billingtest"
	"github.com/mtgban/api-gatewahy/config"
	"github.com/mtgban/mtgban-website/apiproductlist"
	"github.com/stripe/stripe-go/v84"
)

func TestBillingUsageErrors(t *testing.T) {
	d := billingDeps{cat: apiproductlist.MustLoad()}
	cases := []struct {
		name string
		cmd  string
		args []string
		want string
	}{
		{"no verb", "checkout", nil, "usage: api-gatewahy checkout"},
		{"unknown verb", "checkout", []string{"pay"}, "usage: api-gatewahy checkout"},
		{"link without email", "checkout", []string{"link", "-package", "all_data"}, "-email is required"},
		{"link without package", "checkout", []string{"link", "-email", "x@example.com"}, "-package is required"},
		{"invite unknown interval", "invite", []string{"create", "-interval", "weekly"}, "unknown interval"},
		{"invite public interval", "invite", []string{"create", "-interval", "monthly"}, "does not need an invite"},
		{"portal without email", "portal", []string{"link"}, "-email is required"},
		{"plan without package", "plan", []string{"change", "-email", "x@example.com"}, "-package is required"},
		{"bad flag", "stripe", []string{"reconcile", "-bogus"}, "flag provided but not defined"},
	}
	for _, c := range cases {
		var out, errb bytes.Buffer
		if code := runBilling(context.Background(), d, c.cmd, c.args, &out, &errb); code != 2 && code != 1 {
			t.Errorf("%s: exit %d", c.name, code)
		}
		if !strings.Contains(errb.String(), c.want) {
			t.Errorf("%s: stderr %q lacks %q", c.name, errb.String(), c.want)
		}
	}
}

func TestStripeSubscriptionFor(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	stripeActive := apiaccess.Entitlement{Source: apiaccess.SourceStripe, Status: apiaccess.EntitlementActive, ExternalRef: "sub_1"}
	stripeEnded := apiaccess.Entitlement{Source: apiaccess.SourceStripe, Status: apiaccess.EntitlementEnded, ExternalRef: "sub_0"}
	manual := apiaccess.Entitlement{Source: apiaccess.SourceManual, Status: apiaccess.EntitlementActive}
	if id, err := billing.SubscriptionFor([]apiaccess.Entitlement{manual, stripeEnded, stripeActive}); err != nil || id != "sub_1" {
		t.Errorf("one live: %q %v", id, err)
	}
	if _, err := billing.SubscriptionFor([]apiaccess.Entitlement{manual, stripeEnded}); err == nil {
		t.Error("none live accepted")
	}
	other := stripeActive
	other.ExternalRef = "sub_2"
	if _, err := billing.SubscriptionFor([]apiaccess.Entitlement{stripeActive, other}); err == nil || !strings.Contains(err.Error(), "-sub") {
		t.Errorf("two live: %v", err)
	}
	// Grace lapsing does not end the Stripe subscription, so a lapsed row is
	// still a candidate: two of them is still "more than one" to resolve.
	lapsed := other
	lapsed.ValidUntil = &past
	if _, err := billing.SubscriptionFor([]apiaccess.Entitlement{stripeActive, lapsed}); !errors.Is(err, billing.ErrManySubscriptions) {
		t.Errorf("lapsed-but-active row dropped: %v", err)
	}
	// An ended row is never a candidate, lapsed or not.
	endedLapsed := stripeEnded
	endedLapsed.ValidUntil = &past
	if id, err := billing.SubscriptionFor([]apiaccess.Entitlement{endedLapsed, stripeActive}); err != nil || id != "sub_1" {
		t.Errorf("ended row picked: %q %v", id, err)
	}
}

func TestBillingCommandsRegistered(t *testing.T) {
	for _, name := range []string{"catalog", "checkout", "invite", "stripe", "portal", "plan"} {
		if _, ok := commands[name]; !ok {
			t.Errorf("command %s not registered", name)
		}
	}
}

// billingFixture is a store holding x@example.com, the first account and so id 1, and a seeded Stripe.
func billingFixture(t *testing.T, customerID string) (*billingtest.FakeAPI, *apiaccesstest.MemStore, apiaccess.Account) {
	t.Helper()
	ctx := context.Background()
	store := apiaccesstest.New()
	account, err := store.CreateAccount(ctx, "x@example.com", "")
	if err != nil || account.ID != 1 {
		t.Fatalf("account %+v %v", account, err)
	}
	if customerID != "" {
		if account.StripeCustomerID, err = store.SetStripeCustomerID(ctx, account.ID, customerID); err != nil {
			t.Fatal(err)
		}
	}
	return billingtest.SeededFakeAPI(t, apiproductlist.MustLoad()), store, account
}

// stripeRow stores the active stripe entitlement an earlier reconcile left for subID.
func stripeRow(t *testing.T, store *apiaccesstest.MemStore, accountID int64, subID string) {
	t.Helper()
	e := apiaccess.Entitlement{AccountID: accountID, Source: apiaccess.SourceStripe, Games: []string{"magic"}, StoreScope: "ALL_ACCESS", Modes: []string{"retail"}, ExternalRef: subID}
	if _, err := store.UpsertStripeEntitlement(context.Background(), e); err != nil {
		t.Fatal(err)
	}
}

// monthAhead is a subscription period end a month out.
func monthAhead() time.Time { return time.Now().AddDate(0, 1, 0) }

// billingTestDeps builds billingDeps with a magic+pokemon catalog and both fakes wired in.
func billingTestDeps(api *billingtest.FakeAPI, store *apiaccesstest.MemStore) billingDeps {
	cfg := &config.Config{Games: map[string]config.Game{"magic": {}, "pokemon": {}}, Stripe: config.StripeConfig{GraceDays: 10}}
	return billingDeps{store: store, api: api, cfg: cfg, cat: apiproductlist.MustLoad()}
}

// TestPlanChangeKeepsGamesUnlessGiven is issue #33 item 1: plan change must
// start from the subscription's current plan and only override flags given.
func TestPlanChangeKeepsGamesUnlessGiven(t *testing.T) {
	pkgKey, gamesKey := apiproductlist.LookupKey("all_data", "monthly"), apiproductlist.LookupKey("extra_game", "monthly")

	newFixture := func(t *testing.T, subID string) (*billingtest.FakeAPI, *apiaccesstest.MemStore) {
		api, store, account := billingFixture(t, "")
		api.AddSub(t, subID, "cus_1", stripe.SubscriptionStatusActive, map[string]string{
			"package": "all_data", "interval": "monthly", "games": "magic,pokemon", "stores": "", "account_id": "1",
		}, monthAhead(), billingtest.Item{Key: pkgKey, Qty: 1}, billingtest.Item{Key: gamesKey, Qty: 1})
		stripeRow(t, store, account.ID, subID)
		return api, store
	}

	t.Run("games omitted keeps pokemon", func(t *testing.T) {
		api, store := newFixture(t, "sub_1")
		d := billingTestDeps(api, store)
		var out, errb bytes.Buffer
		code := runBilling(context.Background(), d, "plan", []string{"change", "-email", "x@example.com", "-package", "all_data"}, &out, &errb)
		if code != 0 {
			t.Fatalf("exit %d, stderr %q", code, errb.String())
		}
		ups := api.Updates["sub_1"]
		if len(ups) == 0 {
			t.Fatal("no UpdateSubscription call recorded")
		}
		last := ups[len(ups)-1]
		got := last.Metadata["games"]
		if got != "magic,pokemon" {
			t.Errorf("games metadata = %q, want %q (pokemon dropped)", got, "magic,pokemon")
		}
		for _, item := range last.Items {
			if item.Deleted != nil && *item.Deleted {
				t.Errorf("item %s deleted; the extra_game item should survive", stripe.StringValue(item.ID))
			}
		}
	})

	t.Run("games given still changes", func(t *testing.T) {
		api, store := newFixture(t, "sub_2")
		d := billingTestDeps(api, store)
		var out, errb bytes.Buffer
		code := runBilling(context.Background(), d, "plan", []string{"change", "-email", "x@example.com", "-package", "all_data", "-games", "pokemon"}, &out, &errb)
		if code != 0 {
			t.Fatalf("exit %d, stderr %q", code, errb.String())
		}
		ups := api.Updates["sub_2"]
		if got := ups[len(ups)-1].Metadata["games"]; got != "pokemon" {
			t.Errorf("games metadata = %q, want %q", got, "pokemon")
		}
	})

	t.Run("stores cleared from starter to all_data without -stores", func(t *testing.T) {
		starterKey := apiproductlist.LookupKey("starter", "monthly")
		api, store, account := billingFixture(t, "")
		api.AddSub(t, "sub_3", "cus_1", stripe.SubscriptionStatusActive, map[string]string{
			"package": "starter", "interval": "monthly", "games": "magic", "stores": "cardkingdom", "account_id": "1",
		}, monthAhead(), billingtest.Item{Key: starterKey, Qty: 1})
		stripeRow(t, store, account.ID, "sub_3")
		d := billingTestDeps(api, store)

		var out, errb bytes.Buffer
		code := runBilling(context.Background(), d, "plan", []string{"change", "-email", "x@example.com", "-package", "all_data"}, &out, &errb)
		if code != 0 {
			t.Fatalf("exit %d, stderr %q", code, errb.String())
		}
		ups := api.Updates["sub_3"]
		if got := ups[len(ups)-1].Metadata["stores"]; got != "" {
			t.Errorf("stores metadata = %q, want empty (all_data takes no store list)", got)
		}
	})
}

// TestCheckoutLinkPrintsURLOnly is issue #33 item 9: checkout link must
// print the Checkout Session URL alone, not the billing.Session struct.
func TestCheckoutLinkPrintsURLOnly(t *testing.T) {
	api, store, _ := billingFixture(t, "cus_1")
	d := billingTestDeps(api, store)

	var out, errb bytes.Buffer
	code := runBilling(context.Background(), d, "checkout", []string{"link", "-email", "x@example.com", "-package", "all_data"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if len(api.Updates) != 0 {
		t.Fatalf("unexpected UpdateSubscription calls: %v", api.Updates)
	}
	if len(api.SessionIDs) != 1 {
		t.Fatalf("got %d checkout sessions, want 1", len(api.SessionIDs))
	}
	want := fmt.Sprintf("checkout link for x@example.com, valid 24 hours:\n\n    %s\n\n", billingtest.CheckoutURL(api.SessionIDs[0]))
	if out.String() != want {
		t.Errorf("stdout = %q, want %q", out.String(), want)
	}
}

// billingCmd runs one billing command against d and returns its exit code and output.
func billingCmd(t *testing.T, d billingDeps, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runBilling(context.Background(), d, args[0], args[1:], &out, &errb)
	return code, out.String(), errb.String()
}

func TestCatalogSeedCreatesThenMatches(t *testing.T) {
	api := billingtest.NewFakeAPI()
	d := billingTestDeps(api, apiaccesstest.New())
	code, out, errb := billingCmd(t, d, "catalog", "seed")
	if code != 0 {
		t.Fatalf("first seed: exit %d, stderr %q", code, errb)
	}
	for _, want := range []string{"created " + billing.ProductID("all_data") + "\n", "created " + apiproductlist.LookupKey("all_data", "monthly") + "\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("first seed stdout %q lacks %q", out, want)
		}
	}
	if len(api.Products) != len(d.cat.Packages)+len(d.cat.Addons) {
		t.Errorf("%d products, want one per package and add-on", len(api.Products))
	}
	code, out, errb = billingCmd(t, d, "catalog", "seed")
	if code != 0 || out != "Stripe already matches the catalog\n" {
		t.Errorf("second seed: exit %d, stdout %q, stderr %q", code, out, errb)
	}
}

func TestInviteCreate(t *testing.T) {
	store := apiaccesstest.New()
	d := billingTestDeps(billingtest.NewFakeAPI(), store)
	code, out, errb := billingCmd(t, d, "invite", "create", "-interval", "quarterly", "-email", "x@example.com", "-days", "7", "-note", "trial")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb)
	}
	lines := strings.Fields(out)
	token := lines[len(lines)-1]
	inv, ok := store.InviteByToken(token)
	if !ok || store.Calls["CreateInvite"] != 1 {
		t.Fatalf("printed token %q is not the one invite stored (%d created)", token, store.Calls["CreateInvite"])
	}
	if inv.IntervalKey != "quarterly" || inv.Email != "x@example.com" || inv.Note != "trial" {
		t.Errorf("invite %+v", inv)
	}
	// The store keeps microseconds, as Postgres does, so the expiry may round up.
	if left := time.Until(inv.ExpiresAt); left < 6*24*time.Hour || left > 7*24*time.Hour+time.Microsecond {
		t.Errorf("invite expires in %v, want 7 days", left)
	}
	want := fmt.Sprintf("invite for quarterly (x@example.com), expires %s. Shown once, copy it now:\n\n    %s\n\n", inv.ExpiresAt.Format("2006-01-02"), token)
	if out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
}

func TestStripeReconcile(t *testing.T) {
	api, store, account := billingFixture(t, "")
	pkgKey := apiproductlist.LookupKey("all_data", "monthly")
	api.AddSub(t, "sub_1", "cus_1", stripe.SubscriptionStatusActive, map[string]string{
		"package": "all_data", "interval": "monthly", "games": "magic", "stores": "", "account_id": "1",
	}, monthAhead(), billingtest.Item{Key: pkgKey, Qty: 1})
	d := billingTestDeps(api, store)
	rows := func() []apiaccess.Entitlement {
		ents, err := store.ListEntitlements(context.Background(), account.ID)
		if err != nil {
			t.Fatal(err)
		}
		return ents
	}

	code, out, errb := billingCmd(t, d, "stripe", "reconcile", "-sub", "sub_1")
	if code != 0 || out != "subscription sub_1 reconciled\n" {
		t.Fatalf("one: exit %d, stdout %q, stderr %q", code, out, errb)
	}
	if ents := rows(); len(ents) != 1 || ents[0].ExternalRef != "sub_1" || ents[0].Status != apiaccess.EntitlementActive || len(store.Notified) != 1 {
		t.Errorf("one: rows %+v, notified %d", ents, len(store.Notified))
	}

	code, out, errb = billingCmd(t, d, "stripe", "reconcile")
	if code != 0 || out != "stripe reconcile: 1 subscriptions in sync\n" {
		t.Fatalf("all: exit %d, stdout %q, stderr %q", code, out, errb)
	}
	if n := store.Calls["UpsertStripeEntitlement"]; n != 2 || len(rows()) != 1 || len(store.Notified) != 2 {
		t.Errorf("all: %d upserts, %d rows, notified %d, want 2, 1 and 2", n, len(rows()), len(store.Notified))
	}
}

func TestPortalLink(t *testing.T) {
	api, store, _ := billingFixture(t, "cus_1")
	d := billingTestDeps(api, store)
	d.cfg.PublicURL = "https://api.example.com"

	code, out, errb := billingCmd(t, d, "portal", "link", "-email", "x@example.com")
	if code != 0 || out != "portal link for x@example.com:\n\n    https://billing.stripe.test/cus_1\n\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errb)
	}
	if code, _, errb := billingCmd(t, d, "portal", "link", "-email", "x@example.com", "-return", "https://example.com/back"); code != 0 {
		t.Fatalf("with -return: exit %d, stderr %q", code, errb)
	}
	if len(api.Portals) != 2 || stripe.StringValue(api.Portals[0].ReturnURL) != "https://api.example.com" ||
		stripe.StringValue(api.Portals[1].ReturnURL) != "https://example.com/back" {
		t.Errorf("portal sessions %+v, want public_url then -return", api.Portals)
	}
}
