package billing

import (
	"context"
	"errors"
	"reflect"
	"testing"

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
	current, _ := Plan{Package: "starter", Interval: "quarterly", Games: []string{"magic"}, Stores: []string{"CK"}}.Normalize(testCatalog)
	f.addSub(t, "sub_1", "cus_7", stripe.SubscriptionStatusActive, current.Metadata(7), periodEnd, fakeItem{"starter_quarterly", 1})
	rc := &recorder{}

	// Interval "monthly" here is ignored: the subscription is quarterly and stays so.
	got, err := ChangePlan(ctx, f, testCatalog, testGames, testAccount, "sub_1",
		Plan{Package: "all_stores", Interval: "monthly", Games: []string{"magic", "pokemon"}}, rc.reconcile)
	if err != nil {
		t.Fatal(err)
	}
	if got.Package != "all_stores" || got.Interval != "quarterly" || !reflect.DeepEqual(got.Games, []string{"magic", "pokemon"}) {
		t.Errorf("new plan %+v", got)
	}
	ups := f.updates["sub_1"]
	if len(ups) != 1 {
		t.Fatalf("updates %d", len(ups))
	}
	want := []itemChange{
		{id: "si_sub_1_0", deleted: true},
		{price: f.priceByKey("all_stores_quarterly").ID, qty: 1},
		{price: f.priceByKey("extra_game_quarterly").ID, qty: 1},
	}
	if !reflect.DeepEqual(changes(ups[0]), want) {
		t.Errorf("items %+v want %+v", changes(ups[0]), want)
	}
	if stripe.StringValue(ups[0].ProrationBehavior) != "create_prorations" || ups[0].Metadata["package"] != "all_stores" || ups[0].Metadata["interval"] != "quarterly" || ups[0].Metadata["games"] != "magic,pokemon" {
		t.Errorf("params %+v", ups[0])
	}
	if !reflect.DeepEqual(rc.ids, []string{"sub_1"}) {
		t.Errorf("reconciled %v", rc.ids)
	}
}

func TestChangePlanAdjustsQuantities(t *testing.T) {
	f := seededFake(t)
	ctx := context.Background()
	current, _ := Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic", "pokemon"}}.Normalize(testCatalog)
	f.addSub(t, "sub_1", "cus_7", stripe.SubscriptionStatusActive, current.Metadata(7), periodEnd, fakeItem{"all_data_monthly", 1}, fakeItem{"extra_game_monthly", 1})
	rc := &recorder{}
	if _, err := ChangePlan(ctx, f, testCatalog, testGames, testAccount, "sub_1",
		Plan{Package: "all_data", Games: []string{"magic", "pokemon", "lorcana"}}, rc.reconcile); err != nil {
		t.Fatal(err)
	}
	want := []itemChange{{id: "si_sub_1_1", qty: 2}}
	if got := changes(f.updates["sub_1"][0]); !reflect.DeepEqual(got, want) {
		t.Errorf("items %+v want %+v", got, want)
	}
}

func TestChangePlanRefuses(t *testing.T) {
	f := seededFake(t)
	ctx := context.Background()
	current, _ := Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}.Normalize(testCatalog)
	f.addSub(t, "sub_1", "cus_7", stripe.SubscriptionStatusActive, current.Metadata(99), periodEnd, fakeItem{"all_data_monthly", 1})
	rc := &recorder{}
	if _, err := ChangePlan(ctx, f, testCatalog, testGames, testAccount, "sub_1", Plan{Package: "all_stores", Games: []string{"magic"}}, rc.reconcile); err == nil {
		t.Error("another account's subscription was changed")
	}
	if _, err := ChangePlan(ctx, f, testCatalog, testGames, testAccount, "sub_missing", Plan{Package: "all_stores", Games: []string{"magic"}}, rc.reconcile); err == nil {
		t.Error("missing subscription accepted")
	}
	mine, _ := Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}.Normalize(testCatalog)
	f.addSub(t, "sub_2", "cus_7", stripe.SubscriptionStatusActive, mine.Metadata(7), periodEnd, fakeItem{"all_data_monthly", 1})
	if _, err := ChangePlan(ctx, f, testCatalog, testGames, testAccount, "sub_2", Plan{Package: "starter", Games: []string{"magic"}}, rc.reconcile); err == nil {
		t.Error("starter without stores accepted")
	}
	f.fail["UpdateSubscription"] = errors.New("stripe down")
	if _, err := ChangePlan(ctx, f, testCatalog, testGames, testAccount, "sub_2", Plan{Package: "all_stores", Games: []string{"magic"}}, rc.reconcile); err == nil {
		t.Error("stripe failure swallowed")
	}
	if len(rc.ids) != 0 {
		t.Errorf("reconcile ran after failures: %v", rc.ids)
	}
}
