package billing

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
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
		Store: s, API: f, Catalog: testCatalog, Grace: 10 * 24 * time.Hour,
		Alert: func(msg string) { *alerts = append(*alerts, msg) },
		Now:   func() time.Time { return reconNow },
	}
}

func TestMapStatus(t *testing.T) {
	grace := 10 * 24 * time.Hour
	graced := periodEnd.Add(grace)
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
		status, until := MapStatus(c.status, periodEnd, grace, reconNow)
		if status != c.want || (until == nil) != (c.until == nil) || (until != nil && !until.Equal(*c.until)) {
			t.Errorf("%s: got %s %v want %s %v", c.status, status, until, c.want, c.until)
		}
	}
}

func TestReconcileWritesTheRow(t *testing.T) {
	f := seededFake(t)
	s := newMemStore(testAccount)
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	plan, _ := Plan{Package: "starter", Interval: "monthly", Games: []string{"pokemon"}, Stores: []string{"CK", "SCG"}}.Normalize(testCatalog)
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
		!reflect.DeepEqual(e.Games, []string{"magic", "pokemon"}) || e.StoreScope != "TCGLow,TCGMarket,TCGDirect,TCGDirectNet,TCGPlayer,CK,SCG" ||
		!reflect.DeepEqual(e.Modes, []string{"retail", "buylist"}) || !reflect.DeepEqual(e.Addons, []string{"extra_store:1", "extra_game:1"}) ||
		!strings.Contains(e.Note, "TCGplayer plus one store") {
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
	plan, _ := Plan{Package: "all_data", Interval: "monthly"}.Normalize(testCatalog)
	f.addSub(t, "sub_due", "cus_x", stripe.SubscriptionStatusPastDue, plan.Metadata(7), periodEnd, fakeItem{"all_data_monthly", 1})
	f.addSub(t, "sub_gone", "cus_x", stripe.SubscriptionStatusCanceled, plan.Metadata(7), periodEnd, fakeItem{"all_data_monthly", 1})

	if err := r.Subscription(context.Background(), "sub_due"); err != nil {
		t.Fatal(err)
	}
	due := s.ents["sub_due"]
	if due.Status != "active" || due.ValidUntil == nil || !due.ValidUntil.Equal(periodEnd.Add(10*24*time.Hour)) {
		t.Errorf("past_due row %+v", due)
	}
	if err := r.Subscription(context.Background(), "sub_gone"); err != nil {
		t.Fatal(err)
	}
	gone := s.ents["sub_gone"]
	if gone.Status != "ended" || gone.ValidUntil == nil || !gone.ValidUntil.Equal(reconNow) {
		t.Errorf("canceled row %+v", gone)
	}
}

func TestReconcileFindsAccountByCustomer(t *testing.T) {
	f := seededFake(t)
	withCustomer := testAccount
	withCustomer.StripeCustomerID = "cus_7"
	s := newMemStore(withCustomer)
	var alerts []string
	r := newTestReconciler(f, s, &alerts)
	plan, _ := Plan{Package: "all_data", Interval: "monthly"}.Normalize(testCatalog)
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
	plan, _ := Plan{Package: "all_stores", Interval: "monthly", Games: []string{"pokemon"}}.Normalize(testCatalog)
	// The dashboard was used to add a second extra game without touching metadata.
	f.addSub(t, "sub_1", "cus_x", stripe.SubscriptionStatusActive, plan.Metadata(7), periodEnd, fakeItem{"all_stores_monthly", 1}, fakeItem{"extra_game_monthly", 2})
	if err := r.Subscription(context.Background(), "sub_1"); err != nil {
		t.Fatal(err)
	}
	if got := s.ents["sub_1"].Games; !reflect.DeepEqual(got, []string{"magic", "pokemon"}) {
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
	plan, _ := Plan{Package: "all_data", Interval: "monthly"}.Normalize(testCatalog)
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
	if f.calls["GetSubscription"] != 1 {
		t.Errorf("listed subscriptions were refetched: %d", f.calls["GetSubscription"])
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
	plan, _ := Plan{Package: "all_stores", Interval: "monthly"}.Normalize(testCatalog)
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
