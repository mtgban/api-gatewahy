package billing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/stripe/stripe-go/v84"
)

var (
	reconNow  = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	periodEnd = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
)

func newTestReconciler(f *fakeAPI, s *memStore, alerts *[]string) *Reconciler {
	return &Reconciler{
		Store: s, API: f, Catalog: testCatalog, Stores: newFakeStores(), Grace: 10 * 24 * time.Hour,
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
		want   string
		until  *time.Time
	}{
		{stripe.SubscriptionStatusActive, "active", nil},
		{stripe.SubscriptionStatusTrialing, "active", nil},
		{stripe.SubscriptionStatusPastDue, "active", &graced},
		{stripe.SubscriptionStatusCanceled, "ended", &reconNow},
		{stripe.SubscriptionStatusUnpaid, "ended", &reconNow},
		{stripe.SubscriptionStatusIncompleteExpired, "ended", &reconNow},
		{stripe.SubscriptionStatusIncomplete, "ended", &reconNow},
		{stripe.SubscriptionStatusPaused, "ended", &reconNow},
		{"something_new", "ended", &reconNow},
	}
	for _, c := range cases {
		status, until := MapStatus(c.status, anchor, time.Time{}, grace, reconNow)
		if status != c.want || (until == nil) != (c.until == nil) || (until != nil && !until.Equal(*c.until)) {
			t.Errorf("%s: got %s %v want %s %v", c.status, status, until, c.want, c.until)
		}
	}
	endedAt := reconNow.Add(-time.Hour)
	if _, until := MapStatus(stripe.SubscriptionStatusCanceled, anchor, endedAt, grace, reconNow); until == nil || !until.Equal(endedAt) {
		t.Errorf("canceled with ended_at: until %v want %s", until, endedAt)
	}
	if status, until := MapStatus(stripe.SubscriptionStatusActive, anchor, endedAt, grace, reconNow); status != "active" || until != nil {
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
		plan      Plan
		item      fakeItem
		grace     time.Duration
		wantUntil time.Time
		wantAlert bool
	}{
		{
			name:      "monthly",
			plan:      Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}},
			item:      fakeItem{"all_data_monthly", 1},
			grace:     10 * 24 * time.Hour,
			wantUntil: time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC),
		},
		{
			name:      "quarterly",
			plan:      Plan{Package: "starter", Interval: "quarterly", Games: []string{"magic"}, Stores: []string{"cardkingdom"}},
			item:      fakeItem{"starter_quarterly", 1},
			grace:     10 * 24 * time.Hour,
			wantUntil: time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC),
		},
		{
			name:      "yearly",
			plan:      Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}},
			item:      fakeItem{"yearly_test", 1},
			grace:     10 * 24 * time.Hour,
			wantUntil: time.Date(2025, 10, 11, 0, 0, 0, 0, time.UTC),
			wantAlert: true,
		},
		{
			name:      "no grace",
			plan:      Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}},
			item:      fakeItem{"all_data_monthly", 1},
			grace:     0,
			wantUntil: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := seededFake(t)
			f.prices[yearlyPrice.ID] = yearlyPrice
			s := newMemStore(testAccount)
			var alerts []string
			r := newTestReconciler(f, s, &alerts)
			r.Grace = c.grace
			plan, err := c.plan.Normalize(testCatalog)
			if err != nil {
				t.Fatal(err)
			}
			f.addSub(t, "sub_1", "cus_x", stripe.SubscriptionStatusPastDue, plan.Metadata(7), periodEnd, c.item)
			if err := r.Subscription(context.Background(), "sub_1"); err != nil {
				t.Fatal(err)
			}
			e := s.ents["sub_1"]
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
	s := newMemStore(testAccount)
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	plan, _ := Plan{Package: "starter", Interval: "monthly", Games: []string{"magic", "pokemon"}, Stores: []string{"cardkingdom", "starcitygames"}}.Normalize(testCatalog)
	f.addSub(t, "sub_1", "cus_x", stripe.SubscriptionStatusActive, plan.Metadata(7), periodEnd,
		fakeItem{"starter_monthly", 1}, fakeItem{"extra_store_monthly", 1}, fakeItem{"extra_game_monthly", 1})

	if err := r.Subscription(context.Background(), "sub_1"); err != nil {
		t.Fatal(err)
	}
	e, ok := s.ents["sub_1"]
	if !ok {
		t.Fatal("no row written")
	}
	if e.AccountID != 7 || e.Source != "stripe" || e.Status != "active" || e.ValidUntil != nil || e.ExternalRef != "sub_1" ||
		!slices.Equal(e.Games, []string{"magic", "pokemon"}) || e.StoreScope != "CK,CKBLLast,SCG,TCGDirect,TCGDirectNet,TCGLow,TCGMarket,TCGPlayer" ||
		!slices.Equal(e.Modes, []string{"retail", "buylist"}) || !slices.Equal(e.Addons, []string{"extra_store:1", "extra_game:1"}) ||
		!strings.Contains(e.Note, "À la carte") {
		t.Errorf("row %+v", e)
	}
	if s.notified != 1 || len(alerts) != 0 {
		t.Errorf("notified %d alerts %v", s.notified, alerts)
	}

	// Running again converges on the same single row.
	if err := r.Subscription(context.Background(), "sub_1"); err != nil {
		t.Fatal(err)
	}
	if len(s.ents) != 1 || s.ents["sub_1"].ID != e.ID || s.notified != 2 {
		t.Errorf("second run: %d rows, id %d, notified %d", len(s.ents), s.ents["sub_1"].ID, s.notified)
	}
}

func TestReconcileStatusesAndGrace(t *testing.T) {
	f := seededFake(t)
	s := newMemStore(testAccount)
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	plan, _ := Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}.Normalize(testCatalog)
	f.addSub(t, "sub_due", "cus_x", stripe.SubscriptionStatusPastDue, plan.Metadata(7), periodEnd, fakeItem{"all_data_monthly", 1})
	f.addSub(t, "sub_gone", "cus_x", stripe.SubscriptionStatusCanceled, plan.Metadata(7), periodEnd, fakeItem{"all_data_monthly", 1})

	if err := r.Subscription(context.Background(), "sub_due"); err != nil {
		t.Fatal(err)
	}
	due := s.ents["sub_due"]
	// all_data_monthly's period starts a month before periodEnd; grace runs from there.
	wantUntil := periodEnd.AddDate(0, -1, 0).Add(10 * 24 * time.Hour)
	if due.Status != "active" || due.ValidUntil == nil || !due.ValidUntil.Equal(wantUntil) {
		t.Errorf("past_due row %+v, want until %s", due, wantUntil)
	}
	if err := r.Subscription(context.Background(), "sub_gone"); err != nil {
		t.Fatal(err)
	}
	gone := s.ents["sub_gone"]
	if gone.Status != "ended" || gone.ValidUntil == nil || !gone.ValidUntil.Equal(reconNow) {
		t.Errorf("canceled row %+v", gone)
	}
}

// TestPastDueWithNoPeriodStartGetsNoGrace checks a missing current_period_start
// ends access at now, not now plus grace, and does not slide forward on a later reconcile.
func TestPastDueWithNoPeriodStartGetsNoGrace(t *testing.T) {
	f := seededFake(t)
	s := newMemStore(testAccount)
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	plan, _ := Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}.Normalize(testCatalog)
	sub := f.addSub(t, "sub_1", "cus_x", stripe.SubscriptionStatusPastDue, plan.Metadata(7), periodEnd, fakeItem{"all_data_monthly", 1})
	sub.Items.Data[0].CurrentPeriodStart = 0

	if err := r.Subscription(context.Background(), "sub_1"); err != nil {
		t.Fatal(err)
	}
	if e := s.ents["sub_1"]; e.Status != "active" || e.ValidUntil == nil || !e.ValidUntil.Equal(reconNow) {
		t.Errorf("row %+v, want until %s (now, no grace)", e, reconNow)
	}

	r.Now = func() time.Time { return reconNow.Add(72 * time.Hour) }
	if err := r.Subscription(context.Background(), "sub_1"); err != nil {
		t.Fatal(err)
	}
	later := reconNow.Add(72 * time.Hour)
	if got := s.ents["sub_1"].ValidUntil; got == nil || !got.Equal(later) {
		t.Errorf("second reconcile: valid_until %v, want %s (still no grace)", got, later)
	}
}

func TestReconcileFindsAccountByCustomer(t *testing.T) {
	f := seededFake(t)
	withCustomer := testAccount
	withCustomer.StripeCustomerID = "cus_7"
	s := newMemStore(withCustomer)
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	plan, _ := Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}.Normalize(testCatalog)
	md := plan.Metadata(0)
	delete(md, "account_id")
	f.addSub(t, "sub_1", "cus_7", stripe.SubscriptionStatusActive, md, periodEnd, fakeItem{"all_data_monthly", 1})
	if err := r.Subscription(context.Background(), "sub_1"); err != nil {
		t.Fatal(err)
	}
	if s.ents["sub_1"].AccountID != 7 {
		t.Errorf("row %+v", s.ents["sub_1"])
	}

	f.addSub(t, "sub_orphan", "cus_nobody", stripe.SubscriptionStatusActive, plan.Metadata(999), periodEnd, fakeItem{"all_data_monthly", 1})
	err := r.Subscription(context.Background(), "sub_orphan")
	if !errors.Is(err, ErrNoAccount) {
		t.Errorf("orphan: %v", err)
	}
	if _, ok := s.ents["sub_orphan"]; ok {
		t.Error("orphan wrote a row")
	}
	if len(alerts) != 1 || !strings.Contains(alerts[0], "sub_orphan") {
		t.Errorf("alerts %v", alerts)
	}
}

func TestReconcileMetadataWinsOverItems(t *testing.T) {
	f := seededFake(t)
	s := newMemStore(testAccount)
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	plan, _ := Plan{Package: "all_stores", Interval: "monthly", Games: []string{"magic", "pokemon"}}.Normalize(testCatalog)
	// The dashboard was used to add a second extra game without touching metadata.
	f.addSub(t, "sub_1", "cus_x", stripe.SubscriptionStatusActive, plan.Metadata(7), periodEnd, fakeItem{"all_stores_monthly", 1}, fakeItem{"extra_game_monthly", 2})
	if err := r.Subscription(context.Background(), "sub_1"); err != nil {
		t.Fatal(err)
	}
	if got := s.ents["sub_1"].Games; !slices.Equal(got, []string{"magic", "pokemon"}) {
		t.Errorf("games %v", got)
	}
	if len(alerts) != 1 || !strings.Contains(alerts[0], "metadata wins") {
		t.Errorf("alerts %v", alerts)
	}
}

func TestReconcileRejectsBadMetadata(t *testing.T) {
	f := seededFake(t)
	s := newMemStore(testAccount)
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	f.addSub(t, "sub_nometa", "cus_x", stripe.SubscriptionStatusActive, map[string]string{"account_id": "7"}, periodEnd, fakeItem{"all_data_monthly", 1})
	f.addSub(t, "sub_badpkg", "cus_x", stripe.SubscriptionStatusActive, map[string]string{"account_id": "7", "package": "gold", "interval": "monthly"}, periodEnd, fakeItem{"all_data_monthly", 1})
	for _, id := range []string{"sub_nometa", "sub_badpkg", "sub_missing"} {
		if err := r.Subscription(context.Background(), id); err == nil {
			t.Errorf("%s: no error", id)
		}
	}
	if len(s.ents) != 0 {
		t.Errorf("rows written: %v", s.ents)
	}
	if len(alerts) != 2 {
		t.Errorf("alerts %v", alerts)
	}
}

func TestReconcileNeverEndsOnFetchFailure(t *testing.T) {
	f := seededFake(t)
	s := newMemStore(testAccount)
	s.ents["sub_1"] = apiaccess.Entitlement{ID: 1, ExternalRef: "sub_1", Status: "active"}
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	f.fail["GetSubscription"] = errors.New("stripe down")
	if err := r.Subscription(context.Background(), "sub_1"); err == nil {
		t.Error("fetch failure swallowed")
	}
	if s.ents["sub_1"].Status != "active" {
		t.Error("row changed on a fetch failure")
	}
}

// TestResultSummaryCapsErrors checks a long error list is truncated rather
// than producing an unbounded Discord message.
func TestResultSummaryCapsErrors(t *testing.T) {
	res := Result{Checked: 7, Failed: 7}
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
	s := newMemStore(testAccount)
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	plan, _ := Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}.Normalize(testCatalog)
	f.addSub(t, "sub_a", "cus_x", stripe.SubscriptionStatusActive, plan.Metadata(7), periodEnd, fakeItem{"all_data_monthly", 1})
	f.addSub(t, "sub_b", "cus_x", stripe.SubscriptionStatusPastDue, plan.Metadata(7), periodEnd, fakeItem{"all_data_monthly", 1})
	// Canceled in Stripe, so not listed, but still active here: the webhook was missed.
	f.addSub(t, "sub_c", "cus_x", stripe.SubscriptionStatusCanceled, plan.Metadata(7), periodEnd, fakeItem{"all_data_monthly", 1})
	s.ents["sub_c"] = apiaccess.Entitlement{ID: 9, ExternalRef: "sub_c", Status: "active", Source: "stripe"}
	f.addSub(t, "sub_bad", "cus_x", stripe.SubscriptionStatusActive, map[string]string{}, periodEnd, fakeItem{"all_data_monthly", 1})

	res, err := r.All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Checked != 4 || res.Failed != 1 || len(res.Errors) != 1 {
		t.Errorf("result %+v", res)
	}
	if s.ents["sub_a"].Status != "active" || s.ents["sub_b"].Status != "active" || s.ents["sub_c"].Status != "ended" {
		t.Errorf("rows %+v", s.ents)
	}
	// Three listed plus the unlisted sub_c, each fetched once.
	if f.calls["GetSubscription"] != 4 {
		t.Errorf("GetSubscription called %d times, want 4", f.calls["GetSubscription"])
	}
	if !strings.Contains(res.Summary(), "4 subscriptions, 1 failed") {
		t.Errorf("summary %q", res.Summary())
	}
	if ok := (Result{Checked: 2}).Summary(); !strings.Contains(ok, "2 subscriptions in sync") {
		t.Errorf("clean summary %q", ok)
	}
	if s.notified != 1 {
		t.Errorf("notified %d, want exactly one reload for the whole pass", s.notified)
	}

	f.fail["ListSubscriptions"] = errors.New("stripe down")
	if _, err := r.All(context.Background()); err == nil {
		t.Error("list failure swallowed")
	}
}

// TestReconcileToleratesReplacedPrice covers a subscription still holding a
// price Seed replaced: LookupKey is cleared, but the metadata rebuilds it.
func TestReconcileToleratesReplacedPrice(t *testing.T) {
	f := seededFake(t)
	s := newMemStore(testAccount)
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	plan, _ := Plan{Package: "all_stores", Interval: "monthly", Games: []string{"magic"}}.Normalize(testCatalog)
	f.addSub(t, "sub_1", "cus_x", stripe.SubscriptionStatusActive, plan.Metadata(7), periodEnd, fakeItem{"all_stores_monthly", 1})

	// Simulate Seed's replacement path clearing the old price's lookup key.
	price := f.priceByKey("all_stores_monthly")
	price.LookupKey = ""

	if err := r.Subscription(context.Background(), "sub_1"); err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Errorf("alerts %v, want no mismatch alert for a replaced price", alerts)
	}
	if got := priceLookupKey(&stripe.Price{}); got != "" {
		t.Errorf("priceLookupKey with neither lookup key nor metadata: %q", got)
	}
}

func TestReconcileWhenStoresDoNotResolve(t *testing.T) {
	f := seededFake(t)
	s := newMemStore(testAccount)
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	lister := newFakeStores()
	lister.fail = errors.New("connection refused")
	r.Stores = lister
	plan, _ := Plan{Package: "starter", Interval: "monthly", Games: []string{"magic"}, Stores: []string{"cardkingdom"}}.Normalize(testCatalog)
	f.addSub(t, "sub_live", "cus_x", stripe.SubscriptionStatusActive, plan.Metadata(7), periodEnd, fakeItem{"starter_monthly", 1})
	f.addSub(t, "sub_gone", "cus_x", stripe.SubscriptionStatusCanceled, plan.Metadata(7), periodEnd, fakeItem{"starter_monthly", 1})

	if err := r.Subscription(context.Background(), "sub_live"); !errors.Is(err, ErrStoresUnavailable) {
		t.Errorf("active with the site down: %v", err)
	}
	if _, ok := s.ents["sub_live"]; ok {
		t.Error("a row was written without a resolved scope")
	}
	if err := r.Subscription(context.Background(), "sub_gone"); err != nil {
		t.Fatalf("cancelled with the site down: %v", err)
	}
	if e := s.ents["sub_gone"]; e.Status != "ended" || e.StoreScope != "cardkingdom" {
		t.Errorf("cancelled row %+v", e)
	}
	if len(alerts) != 2 {
		t.Errorf("alerts %v", alerts)
	}
}

func TestReconcileAllAlertsOnTwoActiveSubscriptions(t *testing.T) {
	f := seededFake(t)
	s := newMemStore(testAccount, apiaccess.Account{ID: 8, Status: "active"}, apiaccess.Account{ID: 9, Status: "active"})
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	plan, _ := Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}.Normalize(testCatalog)
	f.addSub(t, "sub_a", "cus_x", stripe.SubscriptionStatusActive, plan.Metadata(7), periodEnd, fakeItem{"all_data_monthly", 1})
	f.addSub(t, "sub_b", "cus_x", stripe.SubscriptionStatusActive, plan.Metadata(7), periodEnd, fakeItem{"all_data_monthly", 1})
	f.addSub(t, "sub_c", "cus_y", stripe.SubscriptionStatusActive, plan.Metadata(8), periodEnd, fakeItem{"all_data_monthly", 1})
	f.addSub(t, "sub_d", "cus_z", stripe.SubscriptionStatusActive, plan.Metadata(9), periodEnd, fakeItem{"all_data_monthly", 1})

	res, err := r.All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := "api-gatewahy: account 7 has 2 active Stripe subscriptions (sub_a, sub_b)"
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
	s := newMemStore(testAccount)
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	plan, _ := Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}.Normalize(testCatalog)
	f.addSub(t, "sub_ok", "cus_x", stripe.SubscriptionStatusActive, plan.Metadata(7), periodEnd, fakeItem{"all_data_monthly", 1})
	f.addSub(t, "sub_bad", "cus_x", stripe.SubscriptionStatusActive, map[string]string{}, periodEnd, fakeItem{"all_data_monthly", 1})
	f.addSub(t, "sub_orphan", "cus_nobody", stripe.SubscriptionStatusActive, plan.Metadata(999), periodEnd, fakeItem{"all_data_monthly", 1})

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

func TestIsPermanent(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"no plan metadata", fmt.Errorf("subscription sub_1: %w", ErrNoPlan), true},
		{"unknown package", invalid("unknown package %q", "gold"), true},
		{"no account", fmt.Errorf("subscription sub_1: %w", ErrNoAccount), true},
		{"validation", &ValidationError{Msg: "Store x is not available for the games you picked."}, true},
		{"stores down", fmt.Errorf("%w: magic: refused", ErrStoresUnavailable), false},
		{"stores down wrapping a validation", fmt.Errorf("%w: %w", ErrStoresUnavailable, invalid("odd")), false},
		{"stripe", &stripe.Error{Msg: "api error", HTTPStatusCode: 500}, false},
		{"db", errors.New("billing: upsert sub_1: db down"), false},
		{"cancelled", context.Canceled, false},
	}
	for _, c := range cases {
		if got := isPermanent(c.err); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// TestReconcileEndedKeepsItsEndDate checks an ended row ends when Stripe
// says it did, and a later event does not move that.
func TestReconcileEndedKeepsItsEndDate(t *testing.T) {
	f := seededFake(t)
	s := newMemStore(testAccount)
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	plan, _ := Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}.Normalize(testCatalog)
	sub := f.addSub(t, "sub_gone", "cus_x", stripe.SubscriptionStatusCanceled, plan.Metadata(7), periodEnd, fakeItem{"all_data_monthly", 1})
	endedAt := reconNow.Add(-48 * time.Hour)
	sub.EndedAt = endedAt.Unix()

	for _, now := range []time.Time{reconNow, reconNow.Add(72 * time.Hour)} {
		r.Now = func() time.Time { return now }
		if err := r.Subscription(context.Background(), "sub_gone"); err != nil {
			t.Fatal(err)
		}
		if e := s.ents["sub_gone"]; e.Status != "ended" || e.ValidUntil == nil || !e.ValidUntil.Equal(endedAt) {
			t.Errorf("at %s: row %+v, want ended at %s", now, e, endedAt)
		}
	}
}

// TestReconcileAllLogsEveryFailure checks the log names the failures the summary leaves out.
func TestReconcileAllLogsEveryFailure(t *testing.T) {
	f := seededFake(t)
	s := newMemStore(testAccount)
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	for i := range 7 {
		f.addSub(t, fmt.Sprintf("sub_bad%d", i), "cus_x", stripe.SubscriptionStatusActive, map[string]string{}, periodEnd, fakeItem{"all_data_monthly", 1})
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
	*memStore
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
	return s.memStore.GetAccount(ctx, id)
}

func (s *overlapStore) UpsertStripeEntitlement(ctx context.Context, e apiaccess.Entitlement) (apiaccess.Entitlement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inFlight--
	return s.memStore.UpsertStripeEntitlement(ctx, e)
}

func (s *overlapStore) Notify(ctx context.Context, payload string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.memStore.Notify(ctx, payload)
}

// TestReconcileSerializesPerSubscription runs two reconciles of one
// subscription at once and checks they never overlap.
func TestReconcileSerializesPerSubscription(t *testing.T) {
	f := seededFake(t)
	s := &overlapStore{memStore: newMemStore(testAccount), second: make(chan struct{})}
	var alerts []string
	r := newTestReconciler(f, s.memStore, &alerts)
	r.Store = s
	r.Alert = nil
	plan, _ := Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}.Normalize(testCatalog)
	f.addSub(t, "sub_1", "cus_x", stripe.SubscriptionStatusActive, plan.Metadata(7), periodEnd, fakeItem{"all_data_monthly", 1})

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
	if s.ents["sub_1"].Status != "active" || s.notified != 2 {
		t.Errorf("row %+v, notified %d", s.ents["sub_1"], s.notified)
	}
}

// TestSubLocksAreKeyedAndFreed checks another id does not wait and a released id leaves no entry.
func TestSubLocksAreKeyedAndFreed(t *testing.T) {
	var l subLocks
	unlockA := l.lock("sub_a")
	done := make(chan struct{})
	go func() {
		l.lock("sub_b")()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sub_b waited on sub_a's lock")
	}
	unlockA()
	if len(l.locks) != 0 {
		t.Errorf("%d entries left after every unlock", len(l.locks))
	}
}

// TestSubLocksKeepAnAwaitedEntry checks the holder's unlock leaves the entry
// a waiter holds, so a newcomer queues behind the waiter.
func TestSubLocksKeepAnAwaitedEntry(t *testing.T) {
	var l subLocks
	var mu sync.Mutex
	inFlight, most := 0, 0
	enter := func() {
		mu.Lock()
		defer mu.Unlock()
		inFlight++
		most = max(most, inFlight)
	}
	leave := func() {
		mu.Lock()
		defer mu.Unlock()
		inFlight--
	}
	refs := func() int {
		l.mu.Lock()
		defer l.mu.Unlock()
		if e := l.locks["sub_a"]; e != nil {
			return e.refs
		}
		return 0
	}

	unlockHolder := l.lock("sub_a")
	enter()
	waiterIn, releaseWaiter, waiterDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(waiterDone)
		unlock := l.lock("sub_a")
		enter()
		close(waiterIn)
		<-releaseWaiter
		leave()
		unlock()
	}()
	deadline := time.After(5 * time.Second)
	for refs() != 2 {
		select {
		case <-deadline:
			t.Fatal("the waiter never queued on sub_a")
		default:
			runtime.Gosched()
		}
	}
	leave()
	unlockHolder()
	select {
	case <-waiterIn:
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter never got sub_a")
	}

	newDone := make(chan struct{})
	go func() {
		defer close(newDone)
		unlock := l.lock("sub_a")
		enter()
		leave()
		unlock()
	}()
	select {
	case <-newDone:
		t.Error("the newcomer got sub_a while the waiter held it")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseWaiter)
	for _, done := range []chan struct{}{waiterDone, newDone} {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("a contender never finished")
		}
	}
	if most != 1 || len(l.locks) != 0 {
		t.Errorf("%d held sub_a at once, %d entries left", most, len(l.locks))
	}
}

// gatedAPI holds the first fetch open until release closes and reports each fetch as it starts.
type gatedAPI struct {
	*fakeAPI
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
	return a.fakeAPI.GetSubscription(ctx, id)
}

// TestReconcileSerializesTheFetch holds the first fetch of sub_1 open and
// checks a second reconcile does not fetch until it is released.
func TestReconcileSerializesTheFetch(t *testing.T) {
	f := seededFake(t)
	s := newMemStore(testAccount)
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	r.Alert = nil
	api := &gatedAPI{fakeAPI: f, started: make(chan int, 2), release: make(chan struct{})}
	r.API = api
	plan, _ := Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}.Normalize(testCatalog)
	f.addSub(t, "sub_1", "cus_x", stripe.SubscriptionStatusActive, plan.Metadata(7), periodEnd, fakeItem{"all_data_monthly", 1})

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
	*fakeAPI
	listed []*stripe.Subscription
}

func (a staleListAPI) ListSubscriptions(context.Context) ([]*stripe.Subscription, error) {
	return a.listed, a.enter("ListSubscriptions")
}

// TestReconcileAllRefetchesAStaleListing cancels a subscription after the
// pass listed it; the webhook's ended row must survive the pass.
func TestReconcileAllRefetchesAStaleListing(t *testing.T) {
	f := seededFake(t)
	s := newMemStore(testAccount)
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	plan, _ := Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}.Normalize(testCatalog)
	sub := f.addSub(t, "sub_1", "cus_x", stripe.SubscriptionStatusActive, plan.Metadata(7), periodEnd, fakeItem{"all_data_monthly", 1})
	listed := *sub
	r.API = staleListAPI{fakeAPI: f, listed: []*stripe.Subscription{&listed}}

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
	if e := s.ents["sub_1"]; e.Status != "ended" || e.ValidUntil == nil || !e.ValidUntil.Equal(endedAt) {
		t.Errorf("row %+v, want ended at %s", e, endedAt)
	}
}

// TestReconcileAllAlertsWhenTheDuplicateCheckFails pins that a failed
// post-pass listing alerts and still returns the pass.
func TestReconcileAllAlertsWhenTheDuplicateCheckFails(t *testing.T) {
	f := seededFake(t)
	s := newMemStore(testAccount)
	s.refsErrAt = 2
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	plan, _ := Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}.Normalize(testCatalog)
	f.addSub(t, "sub_a", "cus_x", stripe.SubscriptionStatusActive, plan.Metadata(7), periodEnd, fakeItem{"all_data_monthly", 1})

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
