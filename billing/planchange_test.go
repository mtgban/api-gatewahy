package billing_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/mtgban/api-gatewahy/billing"
	"github.com/mtgban/api-gatewahy/billing/billingtest"
	"github.com/stripe/stripe-go/v84"
)

type itemChange struct {
	id, price string
	qty       int64
	deleted   bool
}

func changes(params *stripe.SubscriptionUpdateParams) []itemChange {
	var out []itemChange
	for _, it := range params.Items {
		out = append(out, itemChange{stripe.StringValue(it.ID), stripe.StringValue(it.Price), stripe.Int64Value(it.Quantity), stripe.BoolValue(it.Deleted)})
	}
	return out
}

func TestChangePlanRewritesItems(t *testing.T) {
	f := seededFake(t)
	ctx := context.Background()
	current, _ := billing.Plan{Package: "starter", Interval: "quarterly", Games: []string{"magic"}, Stores: []string{"cardkingdom"}}.Normalize(testCatalog)
	f.AddSub(t, "sub_1", "cus_7", stripe.SubscriptionStatusActive, current.Metadata(testAccount.ID), periodEnd, billingtest.Item{Key: "starter_quarterly", Qty: 1})
	rc := &recorder{}

	// Interval "monthly" here is ignored: the subscription is quarterly and stays so.
	got, err := billing.ChangePlan(ctx, f, testCatalog, billingtest.NewFakeStores(), testGames, testAccount, "sub_1",
		billing.Plan{Package: "all_stores", Interval: "monthly", Games: []string{"magic", "pokemon"}}, rc.reconcile)
	if err != nil {
		t.Fatal(err)
	}
	if got.Package != "all_stores" || got.Interval != "quarterly" || !slices.Equal(got.Games, []string{"magic", "pokemon"}) {
		t.Errorf("new plan %+v", got)
	}
	ups := f.Updates["sub_1"]
	if len(ups) != 1 {
		t.Fatalf("updates %d", len(ups))
	}
	want := []itemChange{
		{id: "si_sub_1_0", deleted: true},
		{price: f.PriceByKey("all_stores_quarterly").ID, qty: 1},
		{price: f.PriceByKey("extra_game_quarterly").ID, qty: 1},
	}
	if !slices.Equal(changes(ups[0]), want) {
		t.Errorf("items %+v want %+v", changes(ups[0]), want)
	}
	if stripe.StringValue(ups[0].ProrationBehavior) != "always_invoice" || ups[0].Metadata["package"] != "all_stores" || ups[0].Metadata["interval"] != "quarterly" || ups[0].Metadata["games"] != "magic,pokemon" {
		t.Errorf("params %+v", ups[0])
	}
	if !slices.Equal(rc.ids, []string{"sub_1"}) {
		t.Errorf("reconciled %v", rc.ids)
	}
}

func TestChangePlanAdjustsQuantities(t *testing.T) {
	f := seededFake(t)
	ctx := context.Background()
	current, _ := billing.Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic", "pokemon"}}.Normalize(testCatalog)
	f.AddSub(t, "sub_1", "cus_7", stripe.SubscriptionStatusActive, current.Metadata(testAccount.ID), periodEnd, billingtest.Item{Key: "all_data_monthly", Qty: 1}, billingtest.Item{Key: "extra_game_monthly", Qty: 1})
	rc := &recorder{}
	if _, err := billing.ChangePlan(ctx, f, testCatalog, billingtest.NewFakeStores(), testGames, testAccount, "sub_1",
		billing.Plan{Package: "all_data", Games: []string{"magic", "pokemon", "lorcana"}}, rc.reconcile); err != nil {
		t.Fatal(err)
	}
	want := []itemChange{{id: "si_sub_1_1", qty: 2}}
	if got := changes(f.Updates["sub_1"][0]); !slices.Equal(got, want) {
		t.Errorf("items %+v want %+v", got, want)
	}
}

func TestChangePlanRefuses(t *testing.T) {
	f := seededFake(t)
	ctx := context.Background()
	current, _ := billing.Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}.Normalize(testCatalog)
	f.AddSub(t, "sub_1", "cus_7", stripe.SubscriptionStatusActive, current.Metadata(99), periodEnd, billingtest.Item{Key: "all_data_monthly", Qty: 1})
	rc := &recorder{}
	if _, err := billing.ChangePlan(ctx, f, testCatalog, billingtest.NewFakeStores(), testGames, testAccount, "sub_1", billing.Plan{Package: "all_stores", Games: []string{"magic"}}, rc.reconcile); err == nil {
		t.Error("another account's subscription was changed")
	}
	if _, err := billing.ChangePlan(ctx, f, testCatalog, billingtest.NewFakeStores(), testGames, testAccount, "sub_missing", billing.Plan{Package: "all_stores", Games: []string{"magic"}}, rc.reconcile); err == nil {
		t.Error("missing subscription accepted")
	}
	mine, _ := billing.Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}.Normalize(testCatalog)
	f.AddSub(t, "sub_2", "cus_7", stripe.SubscriptionStatusActive, mine.Metadata(testAccount.ID), periodEnd, billingtest.Item{Key: "all_data_monthly", Qty: 1})
	if _, err := billing.ChangePlan(ctx, f, testCatalog, billingtest.NewFakeStores(), testGames, testAccount, "sub_2", billing.Plan{Package: "starter", Games: []string{"magic"}}, rc.reconcile); err == nil {
		t.Error("starter without stores accepted")
	}
	f.Fail["UpdateSubscription"] = errors.New("stripe down")
	if _, err := billing.ChangePlan(ctx, f, testCatalog, billingtest.NewFakeStores(), testGames, testAccount, "sub_2", billing.Plan{Package: "all_stores", Games: []string{"magic"}}, rc.reconcile); err == nil {
		t.Error("stripe failure swallowed")
	}
	if len(rc.ids) != 0 {
		t.Errorf("reconcile ran after failures: %v", rc.ids)
	}
}

// TestChangePlanKeepsReplacedPrice covers an item on a price Seed replaced,
// whose lookup key now lives only in the price metadata.
func TestChangePlanKeepsReplacedPrice(t *testing.T) {
	f := seededFake(t)
	ctx := context.Background()
	current, _ := billing.Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic", "pokemon"}}.Normalize(testCatalog)
	f.AddSub(t, "sub_1", "cus_7", stripe.SubscriptionStatusActive, current.Metadata(testAccount.ID), periodEnd, billingtest.Item{Key: "all_data_monthly", Qty: 1}, billingtest.Item{Key: "extra_game_monthly", Qty: 1})
	old := f.PriceByKey("all_data_monthly")
	if _, err := f.CreatePrice(ctx, &stripe.PriceCreateParams{
		LookupKey: stripe.String(old.LookupKey), TransferLookupKey: stripe.Bool(true), Metadata: old.Metadata,
		UnitAmount: stripe.Int64(old.UnitAmount + 1000), Currency: stripe.String(string(old.Currency)), Product: stripe.String(old.Product.ID),
	}); err != nil {
		t.Fatal(err)
	}
	old.Active = false
	if old.LookupKey != "" {
		t.Fatal("the replacement did not take the lookup key")
	}

	rc := &recorder{}
	if _, err := billing.ChangePlan(ctx, f, testCatalog, billingtest.NewFakeStores(), testGames, testAccount, "sub_1",
		billing.Plan{Package: "all_data", Games: []string{"magic", "pokemon", "lorcana"}}, rc.reconcile); err != nil {
		t.Fatal(err)
	}
	want := []itemChange{{id: "si_sub_1_1", qty: 2}}
	if got := changes(f.Updates["sub_1"][0]); !slices.Equal(got, want) {
		t.Errorf("items %+v want %+v", got, want)
	}
}

func TestChangePlanInvoicesUpgradeImmediately(t *testing.T) {
	f := seededFake(t)
	ctx := context.Background()
	current, _ := billing.Plan{Package: "starter", Interval: "monthly", Games: []string{"magic"}, Stores: []string{"cardkingdom"}}.Normalize(testCatalog)
	f.AddSub(t, "sub_1", "cus_7", stripe.SubscriptionStatusActive, current.Metadata(testAccount.ID), periodEnd, billingtest.Item{Key: "starter_monthly", Qty: 1})
	rc := &recorder{}

	if _, err := billing.ChangePlan(ctx, f, testCatalog, billingtest.NewFakeStores(), testGames, testAccount, "sub_1",
		billing.Plan{Package: "all_stores", Games: []string{"magic"}}, rc.reconcile); err != nil {
		t.Fatal(err)
	}
	ups := f.Updates["sub_1"]
	if len(ups) != 1 {
		t.Fatalf("updates %d", len(ups))
	}
	if got := stripe.StringValue(ups[0].ProrationBehavior); got != "always_invoice" {
		t.Errorf("proration %q, want always_invoice", got)
	}
	if got := stripe.StringValue(ups[0].PaymentBehavior); got != "error_if_incomplete" {
		t.Errorf("payment_behavior %q, want error_if_incomplete", got)
	}
}

func TestChangePlanCreditsDowngradeOnNextInvoice(t *testing.T) {
	f := seededFake(t)
	ctx := context.Background()
	current, _ := billing.Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}.Normalize(testCatalog)
	f.AddSub(t, "sub_1", "cus_7", stripe.SubscriptionStatusActive, current.Metadata(testAccount.ID), periodEnd, billingtest.Item{Key: "all_data_monthly", Qty: 1})
	rc := &recorder{}

	if _, err := billing.ChangePlan(ctx, f, testCatalog, billingtest.NewFakeStores(), testGames, testAccount, "sub_1",
		billing.Plan{Package: "starter", Games: []string{"magic"}, Stores: []string{"cardkingdom"}}, rc.reconcile); err != nil {
		t.Fatal(err)
	}
	ups := f.Updates["sub_1"]
	if len(ups) != 1 {
		t.Fatalf("updates %d", len(ups))
	}
	if got := stripe.StringValue(ups[0].ProrationBehavior); got != "create_prorations" {
		t.Errorf("proration %q, want create_prorations", got)
	}
	if ups[0].PaymentBehavior != nil {
		t.Errorf("payment_behavior %q, want unset on a downgrade", stripe.StringValue(ups[0].PaymentBehavior))
	}
}

// TestChangePlanFailsUpgradeWithoutReconciling checks a declined upgrade
// charge returns an error, never reconciles, and leaves the subscription as it was.
func TestChangePlanFailsUpgradeWithoutReconciling(t *testing.T) {
	f := seededFake(t)
	ctx := context.Background()
	current, _ := billing.Plan{Package: "starter", Interval: "monthly", Games: []string{"magic"}, Stores: []string{"cardkingdom"}}.Normalize(testCatalog)
	f.AddSub(t, "sub_1", "cus_7", stripe.SubscriptionStatusActive, current.Metadata(testAccount.ID), periodEnd, billingtest.Item{Key: "starter_monthly", Qty: 1})
	f.Fail["UpdateSubscription"] = errors.New("card declined")
	rc := &recorder{}

	if _, err := billing.ChangePlan(ctx, f, testCatalog, billingtest.NewFakeStores(), testGames, testAccount, "sub_1",
		billing.Plan{Package: "all_stores", Games: []string{"magic"}}, rc.reconcile); err == nil {
		t.Fatal("declined upgrade charge accepted")
	}
	if len(rc.ids) != 0 {
		t.Errorf("reconcile ran after a failed upgrade: %v", rc.ids)
	}
	if sub := f.Subs["sub_1"]; sub.Metadata["package"] != "starter" {
		t.Errorf("subscription metadata changed to %v despite the failed update", sub.Metadata)
	}
}

// TestChangePlanProratesEqualTotal covers a switch between packages that
// bill the same amount: starter plus two extra stores costs what all_stores
// costs outright, so this is neither an upgrade nor a downgrade.
func TestChangePlanProratesEqualTotal(t *testing.T) {
	f := seededFake(t)
	ctx := context.Background()
	current, _ := billing.Plan{Package: "starter", Interval: "monthly", Games: []string{"magic"},
		Stores: []string{"cardkingdom", "coolstuffinc", "starcitygames"}}.Normalize(testCatalog)
	next, _ := billing.Plan{Package: "all_stores", Interval: "monthly", Games: []string{"magic"}}.Normalize(testCatalog)
	currentTotal, err := current.Total(testCatalog)
	if err != nil {
		t.Fatal(err)
	}
	nextTotal, err := next.Total(testCatalog)
	if err != nil {
		t.Fatal(err)
	}
	// Pin the fixture: a future catalog price change must not turn this into a downgrade.
	if currentTotal != nextTotal {
		t.Fatalf("fixture totals differ: current %d, next %d", currentTotal, nextTotal)
	}
	f.AddSub(t, "sub_1", "cus_7", stripe.SubscriptionStatusActive, current.Metadata(testAccount.ID), periodEnd,
		billingtest.Item{Key: "starter_monthly", Qty: 1}, billingtest.Item{Key: "extra_store_monthly", Qty: 2})
	rc := &recorder{}

	if _, err := billing.ChangePlan(ctx, f, testCatalog, billingtest.NewFakeStores(), testGames, testAccount, "sub_1",
		next, rc.reconcile); err != nil {
		t.Fatal(err)
	}
	ups := f.Updates["sub_1"]
	if len(ups) != 1 {
		t.Fatalf("updates %d", len(ups))
	}
	if got := stripe.StringValue(ups[0].ProrationBehavior); got != "create_prorations" {
		t.Errorf("proration %q, want create_prorations", got)
	}
}

// TestChangePlanReportsPendingAccess separates a change Stripe took but
// reconcile did not apply from a change that never happened.
func TestChangePlanReportsPendingAccess(t *testing.T) {
	f := seededFake(t)
	ctx := context.Background()
	current, _ := billing.Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}.Normalize(testCatalog)
	f.AddSub(t, "sub_1", "cus_7", stripe.SubscriptionStatusActive, current.Metadata(testAccount.ID), periodEnd, billingtest.Item{Key: "all_data_monthly", Qty: 1})
	rc := &recorder{err: errors.New("db down")}
	newPlan := billing.Plan{Package: "all_stores", Games: []string{"magic"}}

	got, err := billing.ChangePlan(ctx, f, testCatalog, billingtest.NewFakeStores(), testGames, testAccount, "sub_1", newPlan, rc.reconcile)
	if !errors.Is(err, billing.ErrChangeNotReconciled) || !strings.Contains(err.Error(), "db down") {
		t.Errorf("reconcile failure after the update: %v", err)
	}
	if got.Package != "all_stores" || len(f.Updates["sub_1"]) != 1 {
		t.Errorf("plan %+v, updates %d", got, len(f.Updates["sub_1"]))
	}

	f.Fail["UpdateSubscription"] = errors.New("stripe down")
	if _, err := billing.ChangePlan(ctx, f, testCatalog, billingtest.NewFakeStores(), testGames, testAccount, "sub_1", newPlan, rc.reconcile); err == nil || errors.Is(err, billing.ErrChangeNotReconciled) {
		t.Errorf("update failure: %v", err)
	}
}
