package billing_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/api-gatewahy/apiaccess/apiaccesstest"
	"github.com/mtgban/api-gatewahy/billing"
	"github.com/mtgban/api-gatewahy/billing/billingtest"
	"github.com/stripe/stripe-go/v84"
)

var (
	reconNow  = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	periodEnd = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
)

// rows reads testAccount's entitlements keyed by subscription id.
func rows(t *testing.T, s *apiaccesstest.MemStore) map[string]apiaccess.Entitlement {
	t.Helper()
	ents, err := s.ListEntitlements(context.Background(), testAccount.ID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]apiaccess.Entitlement{}
	for _, e := range ents {
		out[e.ExternalRef] = e
	}
	return out
}

// seedRow stores an active stripe row for ref, as an earlier reconcile would have.
func seedRow(t *testing.T, s *apiaccesstest.MemStore, ref string) {
	t.Helper()
	e := apiaccess.Entitlement{AccountID: testAccount.ID, Source: apiaccess.SourceStripe, Games: []string{"magic"}, StoreScope: "ALL_ACCESS", Modes: []string{"retail"}, ExternalRef: ref}
	if _, err := s.UpsertStripeEntitlement(context.Background(), e); err != nil {
		t.Fatal(err)
	}
}

func newTestReconciler(f *billingtest.FakeAPI, s *apiaccesstest.MemStore, alerts *[]string) *billing.Reconciler {
	return &billing.Reconciler{
		Store: s, API: f, Catalog: testCatalog, Stores: billingtest.NewFakeStores(), Grace: 10 * 24 * time.Hour,
		Alert: func(msg string) { *alerts = append(*alerts, msg) },
		Now:   func() time.Time { return reconNow },
	}
}

func TestMapStatus(t *testing.T) {
	anchor := periodEnd.AddDate(0, -1, 0) // a period start, not an end
	grace := 10 * 24 * time.Hour
	graced := anchor.Add(grace)
	cases := []struct {
		status stripe.SubscriptionStatus
		want   apiaccess.EntitlementStatus
		until  *time.Time
	}{
		{stripe.SubscriptionStatusActive, apiaccess.EntitlementActive, nil},
		{stripe.SubscriptionStatusTrialing, apiaccess.EntitlementActive, nil},
		{stripe.SubscriptionStatusPastDue, apiaccess.EntitlementActive, &graced},
		{stripe.SubscriptionStatusCanceled, apiaccess.EntitlementEnded, &reconNow},
		{stripe.SubscriptionStatusUnpaid, apiaccess.EntitlementEnded, &reconNow},
		{stripe.SubscriptionStatusIncompleteExpired, apiaccess.EntitlementEnded, &reconNow},
		{stripe.SubscriptionStatusIncomplete, apiaccess.EntitlementEnded, &reconNow},
		{stripe.SubscriptionStatusPaused, apiaccess.EntitlementEnded, &reconNow},
		{"something_new", apiaccess.EntitlementEnded, &reconNow},
	}
	for _, c := range cases {
		status, until := billing.MapStatus(c.status, anchor, time.Time{}, grace, reconNow)
		if status != c.want || (until == nil) != (c.until == nil) || (until != nil && !until.Equal(*c.until)) {
			t.Errorf("%s: got %s %v want %s %v", c.status, status, until, c.want, c.until)
		}
	}
	endedAt := reconNow.Add(-time.Hour)
	if _, until := billing.MapStatus(stripe.SubscriptionStatusCanceled, anchor, endedAt, grace, reconNow); until == nil || !until.Equal(endedAt) {
		t.Errorf("canceled with ended_at: until %v want %s", until, endedAt)
	}
	if status, until := billing.MapStatus(stripe.SubscriptionStatusActive, anchor, endedAt, grace, reconNow); status != "active" || until != nil {
		t.Errorf("active with ended_at: %s %v", status, until)
	}
}

// TestPastDueGraceByInterval runs a past_due subscription through apply per
// interval and pins valid_until to a literal date the old end-based bug could not hit.
func TestPastDueGraceByInterval(t *testing.T) {
	yearlyPrice := &stripe.Price{ID: "price_yearly_test", LookupKey: "yearly_test",
		Recurring: &stripe.PriceRecurring{Interval: stripe.PriceRecurringIntervalYear, IntervalCount: 1}}
	cases := []struct {
		name      string
		plan      billing.Plan
		item      billingtest.Item
		grace     time.Duration
		wantUntil time.Time
		wantAlert bool
	}{
		{
			name:      "monthly",
			plan:      billing.Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}},
			item:      billingtest.Item{Key: "all_data_monthly", Qty: 1},
			grace:     10 * 24 * time.Hour,
			wantUntil: time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC),
		},
		{
			name:      "quarterly",
			plan:      billing.Plan{Package: "starter", Interval: "quarterly", Games: []string{"magic"}, Stores: []string{"cardkingdom"}},
			item:      billingtest.Item{Key: "starter_quarterly", Qty: 1},
			grace:     10 * 24 * time.Hour,
			wantUntil: time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC),
		},
		{
			name:      "yearly",
			plan:      billing.Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}},
			item:      billingtest.Item{Key: "yearly_test", Qty: 1},
			grace:     10 * 24 * time.Hour,
			wantUntil: time.Date(2025, 10, 11, 0, 0, 0, 0, time.UTC),
			wantAlert: true,
		},
		{
			name:      "no grace",
			plan:      billing.Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}},
			item:      billingtest.Item{Key: "all_data_monthly", Qty: 1},
			grace:     0,
			wantUntil: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := seededFake(t)
			f.Prices[yearlyPrice.ID] = yearlyPrice
			s := newStore(t)
			var alerts []string
			r := newTestReconciler(f, s, &alerts)
			r.Grace = c.grace
			plan, err := c.plan.Normalize(testCatalog)
			if err != nil {
				t.Fatal(err)
			}
			f.AddSub(t, "sub_1", "cus_x", stripe.SubscriptionStatusPastDue, plan.Metadata(testAccount.ID), periodEnd, c.item)
			if err := r.Subscription(context.Background(), "sub_1"); err != nil {
				t.Fatal(err)
			}
			e := rows(t, s)["sub_1"]
			if e.Status != "active" || e.ValidUntil == nil || !e.ValidUntil.Equal(c.wantUntil) {
				t.Errorf("row %+v, want until %s", e, c.wantUntil)
			}
			gotAlert := len(alerts) == 1 && strings.Contains(alerts[0], "metadata wins")
			if gotAlert != c.wantAlert {
				t.Errorf("alerts %v, want a metadata-wins alert: %v", alerts, c.wantAlert)
			}
		})
	}
}

func TestReconcileWritesTheRow(t *testing.T) {
	f := seededFake(t)
	s := newStore(t)
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	plan, _ := billing.Plan{Package: "starter", Interval: "monthly", Games: []string{"magic", "pokemon"}, Stores: []string{"cardkingdom", "starcitygames"}}.Normalize(testCatalog)
	f.AddSub(t, "sub_1", "cus_x", stripe.SubscriptionStatusActive, plan.Metadata(testAccount.ID), periodEnd,
		billingtest.Item{Key: "starter_monthly", Qty: 1}, billingtest.Item{Key: "extra_store_monthly", Qty: 1}, billingtest.Item{Key: "extra_game_monthly", Qty: 1})

	if err := r.Subscription(context.Background(), "sub_1"); err != nil {
		t.Fatal(err)
	}
	e, ok := rows(t, s)["sub_1"]
	if !ok {
		t.Fatal("no row written")
	}
	if e.AccountID != testAccount.ID || e.Source != apiaccess.SourceStripe || e.Status != apiaccess.EntitlementActive || e.ValidUntil != nil || e.ExternalRef != "sub_1" ||
		!slices.Equal(e.Games, []string{"magic", "pokemon"}) || e.StoreScope != "CK,CKBLLast,SCG,TCGDirect,TCGDirectNet,TCGLow,TCGMarket,TCGPlayer" ||
		!slices.Equal(e.Modes, []string{"retail", "buylist"}) || !slices.Equal(e.Addons, []string{"extra_store:1", "extra_game:1"}) ||
		!strings.Contains(e.Note, "À la carte") {
		t.Errorf("row %+v", e)
	}
	if len(s.Notified) != 1 || len(alerts) != 0 {
		t.Errorf("notified %d alerts %v", len(s.Notified), alerts)
	}

	// Running again converges on the same single row.
	if err := r.Subscription(context.Background(), "sub_1"); err != nil {
		t.Fatal(err)
	}
	if got := rows(t, s); len(got) != 1 || got["sub_1"].ID != e.ID || len(s.Notified) != 2 {
		t.Errorf("second run: %d rows, id %d, notified %d", len(got), got["sub_1"].ID, len(s.Notified))
	}
}

func TestReconcileStatusesAndGrace(t *testing.T) {
	f := seededFake(t)
	s := newStore(t)
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	plan, _ := billing.Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}.Normalize(testCatalog)
	f.AddSub(t, "sub_due", "cus_x", stripe.SubscriptionStatusPastDue, plan.Metadata(testAccount.ID), periodEnd, billingtest.Item{Key: "all_data_monthly", Qty: 1})
	f.AddSub(t, "sub_gone", "cus_x", stripe.SubscriptionStatusCanceled, plan.Metadata(testAccount.ID), periodEnd, billingtest.Item{Key: "all_data_monthly", Qty: 1})

	if err := r.Subscription(context.Background(), "sub_due"); err != nil {
		t.Fatal(err)
	}
	due := rows(t, s)["sub_due"]
	// all_data_monthly's period starts a month before periodEnd; grace runs from there.
	wantUntil := periodEnd.AddDate(0, -1, 0).Add(10 * 24 * time.Hour)
	if due.Status != apiaccess.EntitlementActive || due.ValidUntil == nil || !due.ValidUntil.Equal(wantUntil) {
		t.Errorf("past_due row %+v, want until %s", due, wantUntil)
	}
	if err := r.Subscription(context.Background(), "sub_gone"); err != nil {
		t.Fatal(err)
	}
	gone := rows(t, s)["sub_gone"]
	if gone.Status != apiaccess.EntitlementEnded || gone.ValidUntil == nil || !gone.ValidUntil.Equal(reconNow) {
		t.Errorf("canceled row %+v", gone)
	}
}

// TestPastDueWithNoPeriodStartGetsNoGrace checks a missing current_period_start
// ends access at now, not now plus grace, and does not slide forward on a later reconcile.
func TestPastDueWithNoPeriodStartGetsNoGrace(t *testing.T) {
	f := seededFake(t)
	s := newStore(t)
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	plan, _ := billing.Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}.Normalize(testCatalog)
	sub := f.AddSub(t, "sub_1", "cus_x", stripe.SubscriptionStatusPastDue, plan.Metadata(testAccount.ID), periodEnd, billingtest.Item{Key: "all_data_monthly", Qty: 1})
	sub.Items.Data[0].CurrentPeriodStart = 0

	if err := r.Subscription(context.Background(), "sub_1"); err != nil {
		t.Fatal(err)
	}
	if e := rows(t, s)["sub_1"]; e.Status != "active" || e.ValidUntil == nil || !e.ValidUntil.Equal(reconNow) {
		t.Errorf("row %+v, want until %s (now, no grace)", e, reconNow)
	}

	r.Now = func() time.Time { return reconNow.Add(72 * time.Hour) }
	if err := r.Subscription(context.Background(), "sub_1"); err != nil {
		t.Fatal(err)
	}
	later := reconNow.Add(72 * time.Hour)
	if got := rows(t, s)["sub_1"].ValidUntil; got == nil || !got.Equal(later) {
		t.Errorf("second reconcile: valid_until %v, want %s (still no grace)", got, later)
	}
}

func TestReconcileFindsAccountByCustomer(t *testing.T) {
	f := seededFake(t)
	s := newStore(t)
	if _, err := s.SetStripeCustomerID(context.Background(), testAccount.ID, "cus_7"); err != nil {
		t.Fatal(err)
	}
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	plan, _ := billing.Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}.Normalize(testCatalog)
	md := plan.Metadata(0)
	delete(md, "account_id")
	f.AddSub(t, "sub_1", "cus_7", stripe.SubscriptionStatusActive, md, periodEnd, billingtest.Item{Key: "all_data_monthly", Qty: 1})
	if err := r.Subscription(context.Background(), "sub_1"); err != nil {
		t.Fatal(err)
	}
	if row := rows(t, s)["sub_1"]; row.AccountID != testAccount.ID {
		t.Errorf("row %+v", row)
	}

	f.AddSub(t, "sub_orphan", "cus_nobody", stripe.SubscriptionStatusActive, plan.Metadata(999), periodEnd, billingtest.Item{Key: "all_data_monthly", Qty: 1})
	err := r.Subscription(context.Background(), "sub_orphan")
	if !errors.Is(err, billing.ErrNoAccount) {
		t.Errorf("orphan: %v", err)
	}
	if _, ok := rows(t, s)["sub_orphan"]; ok {
		t.Error("orphan wrote a row")
	}
	if len(alerts) != 1 || !strings.Contains(alerts[0], "sub_orphan") {
		t.Errorf("alerts %v", alerts)
	}
}

func TestReconcileMetadataWinsOverItems(t *testing.T) {
	f := seededFake(t)
	s := newStore(t)
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	plan, _ := billing.Plan{Package: "all_stores", Interval: "monthly", Games: []string{"magic", "pokemon"}}.Normalize(testCatalog)
	// The dashboard was used to add a second extra game without touching metadata.
	f.AddSub(t, "sub_1", "cus_x", stripe.SubscriptionStatusActive, plan.Metadata(testAccount.ID), periodEnd, billingtest.Item{Key: "all_stores_monthly", Qty: 1}, billingtest.Item{Key: "extra_game_monthly", Qty: 2})
	if err := r.Subscription(context.Background(), "sub_1"); err != nil {
		t.Fatal(err)
	}
	if got := rows(t, s)["sub_1"].Games; !slices.Equal(got, []string{"magic", "pokemon"}) {
		t.Errorf("games %v", got)
	}
	if len(alerts) != 1 || !strings.Contains(alerts[0], "metadata wins") {
		t.Errorf("alerts %v", alerts)
	}
}

func TestReconcileRejectsBadMetadata(t *testing.T) {
	f := seededFake(t)
	s := newStore(t)
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	f.AddSub(t, "sub_nometa", "cus_x", stripe.SubscriptionStatusActive, map[string]string{"account_id": testAccountRef}, periodEnd, billingtest.Item{Key: "all_data_monthly", Qty: 1})
	f.AddSub(t, "sub_badpkg", "cus_x", stripe.SubscriptionStatusActive, map[string]string{"account_id": testAccountRef, "package": "gold", "interval": "monthly"}, periodEnd, billingtest.Item{Key: "all_data_monthly", Qty: 1})
	for _, id := range []string{"sub_nometa", "sub_badpkg", "sub_missing"} {
		if err := r.Subscription(context.Background(), id); err == nil {
			t.Errorf("%s: no error", id)
		}
	}
	if got := rows(t, s); len(got) != 0 {
		t.Errorf("rows written: %v", got)
	}
	if len(alerts) != 2 {
		t.Errorf("alerts %v", alerts)
	}
}

func TestReconcileNeverEndsOnFetchFailure(t *testing.T) {
	f := seededFake(t)
	s := newStore(t)
	seedRow(t, s, "sub_1")
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	f.Fail["GetSubscription"] = errors.New("stripe down")
	if err := r.Subscription(context.Background(), "sub_1"); err == nil {
		t.Error("fetch failure swallowed")
	}
	if rows(t, s)["sub_1"].Status != apiaccess.EntitlementActive {
		t.Error("row changed on a fetch failure")
	}
}

// TestResultSummaryCapsErrors checks a long error list is truncated rather
// than producing an unbounded Discord message.
func TestResultSummaryCapsErrors(t *testing.T) {
	res := billing.Result{Checked: 7, Failed: 7}
	for i := 0; i < 7; i++ {
		res.Errors = append(res.Errors, fmt.Sprintf("err%d", i))
	}
	summary := res.Summary()
	for i := 0; i < 5; i++ {
		if !strings.Contains(summary, fmt.Sprintf("err%d", i)) {
			t.Errorf("summary missing err%d: %q", i, summary)
		}
	}
	for i := 5; i < 7; i++ {
		if strings.Contains(summary, fmt.Sprintf("err%d", i)) {
			t.Errorf("summary should not mention err%d: %q", i, summary)
		}
	}
	if !strings.HasSuffix(summary, "(+2 more)") {
		t.Errorf("summary %q, want suffix (+2 more)", summary)
	}
}

func TestReconcileAll(t *testing.T) {
	f := seededFake(t)
	s := newStore(t)
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	plan, _ := billing.Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}.Normalize(testCatalog)
	f.AddSub(t, "sub_a", "cus_x", stripe.SubscriptionStatusActive, plan.Metadata(testAccount.ID), periodEnd, billingtest.Item{Key: "all_data_monthly", Qty: 1})
	f.AddSub(t, "sub_b", "cus_x", stripe.SubscriptionStatusPastDue, plan.Metadata(testAccount.ID), periodEnd, billingtest.Item{Key: "all_data_monthly", Qty: 1})
	// Canceled in Stripe, so not listed, but still active here: the webhook was missed.
	f.AddSub(t, "sub_c", "cus_x", stripe.SubscriptionStatusCanceled, plan.Metadata(testAccount.ID), periodEnd, billingtest.Item{Key: "all_data_monthly", Qty: 1})
	seedRow(t, s, "sub_c")
	f.AddSub(t, "sub_bad", "cus_x", stripe.SubscriptionStatusActive, map[string]string{}, periodEnd, billingtest.Item{Key: "all_data_monthly", Qty: 1})

	res, err := r.All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Checked != 4 || res.Failed != 1 || len(res.Errors) != 1 {
		t.Errorf("result %+v", res)
	}
	if got := rows(t, s); got["sub_a"].Status != apiaccess.EntitlementActive || got["sub_b"].Status != apiaccess.EntitlementActive || got["sub_c"].Status != apiaccess.EntitlementEnded {
		t.Errorf("rows %+v", got)
	}
	// Three listed plus the unlisted sub_c, each fetched once.
	if f.Calls["GetSubscription"] != 4 {
		t.Errorf("GetSubscription called %d times, want 4", f.Calls["GetSubscription"])
	}
	if !strings.Contains(res.Summary(), "4 subscriptions, 1 failed") {
		t.Errorf("summary %q", res.Summary())
	}
	if ok := (billing.Result{Checked: 2}).Summary(); !strings.Contains(ok, "2 subscriptions in sync") {
		t.Errorf("clean summary %q", ok)
	}
	if len(s.Notified) != 1 {
		t.Errorf("notified %d, want exactly one reload for the whole pass", len(s.Notified))
	}

	f.Fail["ListSubscriptions"] = errors.New("stripe down")
	if _, err := r.All(context.Background()); err == nil {
		t.Error("list failure swallowed")
	}
}

// TestReconcileToleratesReplacedPrice covers a subscription still holding a
// price Seed replaced: LookupKey is cleared, but the metadata rebuilds it.
func TestReconcileToleratesReplacedPrice(t *testing.T) {
	f := seededFake(t)
	s := newStore(t)
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	plan, _ := billing.Plan{Package: "all_stores", Interval: "monthly", Games: []string{"magic"}}.Normalize(testCatalog)
	f.AddSub(t, "sub_1", "cus_x", stripe.SubscriptionStatusActive, plan.Metadata(testAccount.ID), periodEnd, billingtest.Item{Key: "all_stores_monthly", Qty: 1})

	// Simulate Seed's replacement path clearing the old price's lookup key.
	price := f.PriceByKey("all_stores_monthly")
	price.LookupKey = ""

	if err := r.Subscription(context.Background(), "sub_1"); err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Errorf("alerts %v, want no mismatch alert for a replaced price", alerts)
	}
	if got := billing.PriceLookupKey(&stripe.Price{}); got != "" {
		t.Errorf("priceLookupKey with neither lookup key nor metadata: %q", got)
	}
}

func TestReconcileWhenStoresDoNotResolve(t *testing.T) {
	f := seededFake(t)
	s := newStore(t)
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	lister := billingtest.NewFakeStores()
	lister.Fail = errors.New("connection refused")
	r.Stores = lister
	plan, _ := billing.Plan{Package: "starter", Interval: "monthly", Games: []string{"magic"}, Stores: []string{"cardkingdom"}}.Normalize(testCatalog)
	f.AddSub(t, "sub_live", "cus_x", stripe.SubscriptionStatusActive, plan.Metadata(testAccount.ID), periodEnd, billingtest.Item{Key: "starter_monthly", Qty: 1})
	f.AddSub(t, "sub_gone", "cus_x", stripe.SubscriptionStatusCanceled, plan.Metadata(testAccount.ID), periodEnd, billingtest.Item{Key: "starter_monthly", Qty: 1})

	if err := r.Subscription(context.Background(), "sub_live"); !errors.Is(err, billing.ErrStoresUnavailable) {
		t.Errorf("active with the site down: %v", err)
	}
	if _, ok := rows(t, s)["sub_live"]; ok {
		t.Error("a row was written without a resolved scope")
	}
	if err := r.Subscription(context.Background(), "sub_gone"); err != nil {
		t.Fatalf("cancelled with the site down: %v", err)
	}
	if e := rows(t, s)["sub_gone"]; e.Status != apiaccess.EntitlementEnded || e.StoreScope != "cardkingdom" {
		t.Errorf("cancelled row %+v", e)
	}
	if len(alerts) != 2 {
		t.Errorf("alerts %v", alerts)
	}
}

func TestReconcileAllAlertsOnTwoActiveSubscriptions(t *testing.T) {
	f := seededFake(t)
	s := newStore(t)
	var others []int64
	for _, email := range []string{"c@example.com", "d@example.com"} {
		a, err := s.CreateAccount(context.Background(), email, "")
		if err != nil {
			t.Fatal(err)
		}
		others = append(others, a.ID)
	}
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	plan, _ := billing.Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}.Normalize(testCatalog)
	f.AddSub(t, "sub_a", "cus_x", stripe.SubscriptionStatusActive, plan.Metadata(testAccount.ID), periodEnd, billingtest.Item{Key: "all_data_monthly", Qty: 1})
	f.AddSub(t, "sub_b", "cus_x", stripe.SubscriptionStatusActive, plan.Metadata(testAccount.ID), periodEnd, billingtest.Item{Key: "all_data_monthly", Qty: 1})
	f.AddSub(t, "sub_c", "cus_y", stripe.SubscriptionStatusActive, plan.Metadata(others[0]), periodEnd, billingtest.Item{Key: "all_data_monthly", Qty: 1})
	f.AddSub(t, "sub_d", "cus_z", stripe.SubscriptionStatusActive, plan.Metadata(others[1]), periodEnd, billingtest.Item{Key: "all_data_monthly", Qty: 1})

	res, err := r.All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("api-gatewahy: account %d has 2 active Stripe subscriptions (sub_a, sub_b)", testAccount.ID)
	if len(alerts) != 1 || alerts[0] != want {
		t.Errorf("alerts %q, want only %q", alerts, want)
	}
	if res.Duplicates != 1 || res.Failed != 0 || !strings.Contains(res.Summary(), "1 account with more than one subscription") {
		t.Errorf("result %+v, summary %q", res, res.Summary())
	}
}

// TestReconcileAllAlertsOncePerFailure counts what a nightly pass posts:
// its alerts, then the summary serve.go sends.
func TestReconcileAllAlertsOncePerFailure(t *testing.T) {
	f := seededFake(t)
	s := newStore(t)
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	plan, _ := billing.Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}.Normalize(testCatalog)
	f.AddSub(t, "sub_ok", "cus_x", stripe.SubscriptionStatusActive, plan.Metadata(testAccount.ID), periodEnd, billingtest.Item{Key: "all_data_monthly", Qty: 1})
	f.AddSub(t, "sub_bad", "cus_x", stripe.SubscriptionStatusActive, map[string]string{}, periodEnd, billingtest.Item{Key: "all_data_monthly", Qty: 1})
	f.AddSub(t, "sub_orphan", "cus_nobody", stripe.SubscriptionStatusActive, plan.Metadata(999), periodEnd, billingtest.Item{Key: "all_data_monthly", Qty: 1})

	res, err := r.All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	posted := append(slices.Clone(alerts), res.Summary())
	for sub, cause := range map[string]string{"sub_bad": "carries no plan", "sub_orphan": "no known account"} {
		n := 0
		for _, msg := range posted {
			if strings.Contains(msg, cause) {
				n++
			}
		}
		if n != 1 || !strings.Contains(res.Summary(), sub+": ") {
			t.Errorf("%s posted %d times, want once and named: %q", sub, n, posted)
		}
	}
}

// TestReconcileEndedKeepsItsEndDate checks an ended row ends when Stripe
// says it did, and a later event does not move that.
func TestReconcileEndedKeepsItsEndDate(t *testing.T) {
	f := seededFake(t)
	s := newStore(t)
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	plan, _ := billing.Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}.Normalize(testCatalog)
	sub := f.AddSub(t, "sub_gone", "cus_x", stripe.SubscriptionStatusCanceled, plan.Metadata(testAccount.ID), periodEnd, billingtest.Item{Key: "all_data_monthly", Qty: 1})
	endedAt := reconNow.Add(-48 * time.Hour)
	sub.EndedAt = endedAt.Unix()

	for _, now := range []time.Time{reconNow, reconNow.Add(72 * time.Hour)} {
		r.Now = func() time.Time { return now }
		if err := r.Subscription(context.Background(), "sub_gone"); err != nil {
			t.Fatal(err)
		}
		if e := rows(t, s)["sub_gone"]; e.Status != "ended" || e.ValidUntil == nil || !e.ValidUntil.Equal(endedAt) {
			t.Errorf("at %s: row %+v, want ended at %s", now, e, endedAt)
		}
	}
}

// TestReconcileAllLogsEveryFailure checks the log names the failures the summary leaves out.
func TestReconcileAllLogsEveryFailure(t *testing.T) {
	f := seededFake(t)
	s := newStore(t)
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	for i := range 7 {
		f.AddSub(t, fmt.Sprintf("sub_bad%d", i), "cus_x", stripe.SubscriptionStatusActive, map[string]string{}, periodEnd, billingtest.Item{Key: "all_data_monthly", Qty: 1})
	}
	var logged bytes.Buffer
	saved := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(saved) })

	res, err := r.All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed != 7 || !strings.Contains(res.Summary(), "(+2 more)") {
		t.Fatalf("result %+v", res)
	}
	for i := range 7 {
		if id := fmt.Sprintf("sub_bad%d", i); strings.Count(logged.String(), "reconcile "+id+":") != 1 {
			t.Errorf("%s not logged once: %q", id, logged.String())
		}
	}
}

// overlapStore counts applies between the account read and the row write.
type overlapStore struct {
	*apiaccesstest.MemStore
	mu       sync.Mutex
	inFlight int
	most     int
	waited   bool
	second   chan struct{}
}

// GetAccount holds the first apply until a second one arrives, or a bounded guard runs out.
func (s *overlapStore) GetAccount(ctx context.Context, id int64) (apiaccess.Account, error) {
	s.mu.Lock()
	s.inFlight++
	s.most = max(s.most, s.inFlight)
	wait := !s.waited
	s.waited = true
	if s.inFlight == 2 {
		close(s.second)
	}
	s.mu.Unlock()
	if wait {
		select {
		case <-s.second:
		case <-time.After(200 * time.Millisecond):
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.MemStore.GetAccount(ctx, id)
}

func (s *overlapStore) UpsertStripeEntitlement(ctx context.Context, e apiaccess.Entitlement) (apiaccess.Entitlement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inFlight--
	return s.MemStore.UpsertStripeEntitlement(ctx, e)
}

func (s *overlapStore) Notify(ctx context.Context, payload string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.MemStore.Notify(ctx, payload)
}

// TestReconcileSerializesPerSubscription runs two reconciles of one
// subscription at once and checks they never overlap.
func TestReconcileSerializesPerSubscription(t *testing.T) {
	f := seededFake(t)
	s := &overlapStore{MemStore: newStore(t), second: make(chan struct{})}
	var alerts []string
	r := newTestReconciler(f, s.MemStore, &alerts)
	r.Store = s
	r.Alert = nil
	plan, _ := billing.Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}.Normalize(testCatalog)
	f.AddSub(t, "sub_1", "cus_x", stripe.SubscriptionStatusActive, plan.Metadata(testAccount.ID), periodEnd, billingtest.Item{Key: "all_data_monthly", Qty: 1})

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Go(func() { errs[i] = r.Subscription(context.Background(), "sub_1") })
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if s.most != 1 {
		t.Errorf("%d applies of sub_1 ran at once, want 1", s.most)
	}
	if row := rows(t, s.MemStore)["sub_1"]; row.Status != "active" || len(s.Notified) != 2 {
		t.Errorf("row %+v, notified %d", row, len(s.Notified))
	}
}

// gatedAPI holds the first fetch open until release closes and reports each fetch as it starts.
type gatedAPI struct {
	*billingtest.FakeAPI
	mu      sync.Mutex
	fetches int
	started chan int
	release chan struct{}
}

func (a *gatedAPI) GetSubscription(ctx context.Context, id string) (*stripe.Subscription, error) {
	a.mu.Lock()
	a.fetches++
	n := a.fetches
	a.mu.Unlock()
	a.started <- n
	if n == 1 {
		<-a.release
	}
	return a.FakeAPI.GetSubscription(ctx, id)
}

// TestReconcileSerializesTheFetch holds the first fetch of sub_1 open and
// checks a second reconcile does not fetch until it is released.
func TestReconcileSerializesTheFetch(t *testing.T) {
	f := seededFake(t)
	s := newStore(t)
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	r.Alert = nil
	api := &gatedAPI{FakeAPI: f, started: make(chan int, 2), release: make(chan struct{})}
	r.API = api
	plan, _ := billing.Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}.Normalize(testCatalog)
	f.AddSub(t, "sub_1", "cus_x", stripe.SubscriptionStatusActive, plan.Metadata(testAccount.ID), periodEnd, billingtest.Item{Key: "all_data_monthly", Qty: 1})

	errs := make(chan error, 2)
	reconcile := func() { errs <- r.Subscription(context.Background(), "sub_1") }
	go reconcile()
	select {
	case <-api.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first fetch never started")
	}
	go reconcile()
	early := false
	select {
	case <-api.started:
		early = true
		t.Error("the second fetch started while the first held sub_1's lock")
	case <-time.After(100 * time.Millisecond):
	}
	close(api.release)
	if !early {
		select {
		case <-api.started:
		case <-time.After(5 * time.Second):
			t.Fatal("the second fetch never started")
		}
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

// staleListAPI lists a snapshot taken before Stripe changed.
type staleListAPI struct {
	*billingtest.FakeAPI
	listed []*stripe.Subscription
}

func (a staleListAPI) ListSubscriptions(ctx context.Context) ([]*stripe.Subscription, error) {
	_, err := a.FakeAPI.ListSubscriptions(ctx)
	return a.listed, err
}

// TestReconcileAllRefetchesAStaleListing cancels a subscription after the
// pass listed it; the webhook's ended row must survive the pass.
func TestReconcileAllRefetchesAStaleListing(t *testing.T) {
	f := seededFake(t)
	s := newStore(t)
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	plan, _ := billing.Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}.Normalize(testCatalog)
	sub := f.AddSub(t, "sub_1", "cus_x", stripe.SubscriptionStatusActive, plan.Metadata(testAccount.ID), periodEnd, billingtest.Item{Key: "all_data_monthly", Qty: 1})
	listed := *sub
	r.API = staleListAPI{FakeAPI: f, listed: []*stripe.Subscription{&listed}}

	endedAt := reconNow.Add(-time.Hour)
	sub.Status = stripe.SubscriptionStatusCanceled
	sub.EndedAt = endedAt.Unix()
	if err := r.Subscription(context.Background(), "sub_1"); err != nil {
		t.Fatal(err)
	}

	res, err := r.All(context.Background())
	if err != nil || res.Failed != 0 {
		t.Fatalf("result %+v, err %v", res, err)
	}
	if e := rows(t, s)["sub_1"]; e.Status != "ended" || e.ValidUntil == nil || !e.ValidUntil.Equal(endedAt) {
		t.Errorf("row %+v, want ended at %s", e, endedAt)
	}
}

// TestReconcileAllAlertsWhenTheDuplicateCheckFails pins that a failed
// post-pass listing alerts and still returns the pass.
func TestReconcileAllAlertsWhenTheDuplicateCheckFails(t *testing.T) {
	f := seededFake(t)
	s := newStore(t)
	s.Fail["ListActiveStripeRefs"], s.FailAt["ListActiveStripeRefs"] = errors.New("refs down"), 2
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	plan, _ := billing.Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}.Normalize(testCatalog)
	f.AddSub(t, "sub_a", "cus_x", stripe.SubscriptionStatusActive, plan.Metadata(testAccount.ID), periodEnd, billingtest.Item{Key: "all_data_monthly", Qty: 1})

	res, err := r.All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := "api-gatewahy: duplicate subscription check: refs down"
	if len(alerts) != 1 || alerts[0] != want {
		t.Errorf("alerts %q, want only %q", alerts, want)
	}
	if res.Checked != 1 || res.Failed != 0 || res.Duplicates != 0 {
		t.Errorf("result %+v", res)
	}
}
