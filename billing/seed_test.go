package billing

import (
	"context"
	"reflect"
	"testing"

	"github.com/mtgban/mtgban-website/apiproductlist"
	"github.com/stripe/stripe-go/v84"
)

func TestSeedCreatesEverythingOnce(t *testing.T) {
	f := newFakeAPI()
	ctx := context.Background()
	res, err := Seed(ctx, f, testCatalog)
	if err != nil {
		t.Fatal(err)
	}
	wantItems := len(testCatalog.Packages) + len(testCatalog.Addons)
	wantPrices := wantItems * len(testCatalog.Intervals)
	if len(res.Created) != wantItems+wantPrices || len(res.Updated) != 0 || len(res.Archived) != 0 {
		t.Errorf("first seed: %+v", res)
	}
	if len(f.products) != wantItems || len(f.prices) != wantPrices {
		t.Errorf("fake holds %d products %d prices", len(f.products), len(f.prices))
	}
	p := f.priceByKey("starter_quarterly")
	if p == nil || p.UnitAmount != 60000 || p.Product.ID != ProductID("starter") || p.Recurring.Interval != "month" || p.Recurring.IntervalCount != 3 ||
		p.Currency != "usd" || p.Metadata["package"] != "starter" || p.Metadata["store_scope"] != "explicit" || p.Metadata["modes"] != "retail,buylist" || p.Metadata["interval_key"] != "quarterly" {
		t.Errorf("starter_quarterly %+v", p)
	}
	a := f.priceByKey("extra_game_monthly")
	if a == nil || a.UnitAmount != 15000 || a.Metadata["kind"] != "addon" || a.Metadata["addon"] != "extra_game" || a.Metadata["applies_to"] != "starter,all_stores,all_data" {
		t.Errorf("extra_game_monthly %+v", a)
	}
	if prod := f.products[ProductID("all_data")]; prod == nil || prod.Name != "All data" || prod.Metadata["kind"] != "package" {
		t.Errorf("all_data product %+v", prod)
	}

	second, err := Seed(ctx, f, testCatalog)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Created)+len(second.Updated)+len(second.Archived) != 0 {
		t.Errorf("second seed changed things: %+v", second)
	}
}

func TestSeedReplacesChangedAmount(t *testing.T) {
	f := newFakeAPI()
	ctx := context.Background()
	if _, err := Seed(ctx, f, testCatalog); err != nil {
		t.Fatal(err)
	}
	old := f.priceByKey("all_stores_monthly")

	changed := *testCatalog
	changed.Packages = append([]apiproductlist.Package(nil), testCatalog.Packages...)
	for i := range changed.Packages {
		if changed.Packages[i].Key == "all_stores" {
			changed.Packages[i].Monthly = 55000
		}
	}
	res, err := Seed(ctx, f, &changed)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Created, []string{"all_stores_monthly", "all_stores_quarterly"}) || len(res.Archived) != 2 || len(res.Updated) != 0 {
		t.Errorf("replace: %+v", res)
	}
	if old.Active || old.LookupKey != "" {
		t.Errorf("old price still live: %+v", old)
	}
	if now := f.priceByKey("all_stores_monthly"); now == nil || now.ID == old.ID || now.UnitAmount != 55000 {
		t.Errorf("replacement %+v", now)
	}
}

func TestSeedRefreshesDriftAndReactivates(t *testing.T) {
	f := newFakeAPI()
	ctx := context.Background()
	if _, err := Seed(ctx, f, testCatalog); err != nil {
		t.Fatal(err)
	}
	p := f.priceByKey("all_data_monthly")
	p.Active = false
	p.Metadata = map[string]string{"stale": "yes"}
	f.products[ProductID("extra_store")].Active = false
	f.products[ProductID("starter")].Name = "renamed by hand"

	res, err := Seed(ctx, f, testCatalog)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Updated, []string{ProductID("starter"), ProductID("extra_store"), "all_data_monthly"}) || len(res.Created) != 0 || len(res.Archived) != 0 {
		t.Errorf("refresh: %+v", res)
	}
	if !p.Active || p.Metadata["package"] != "all_data" || p.Metadata["stale"] != "" {
		t.Errorf("price not refreshed: %+v", p)
	}
	if !f.products[ProductID("extra_store")].Active || f.products[ProductID("starter")].Name != "TCGplayer plus one store" {
		t.Error("products not refreshed")
	}
}

func TestSeedIgnoresExtraMetadata(t *testing.T) {
	f := newFakeAPI()
	ctx := context.Background()
	if _, err := Seed(ctx, f, testCatalog); err != nil {
		t.Fatal(err)
	}
	p := f.priceByKey("starter_monthly")
	p.Metadata["note"] = "set by hand"

	res, err := Seed(ctx, f, testCatalog)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created)+len(res.Updated)+len(res.Archived) != 0 {
		t.Errorf("extra metadata key triggered a change: %+v", res)
	}

	p.Metadata["modes"] = "retail"
	res, err = Seed(ctx, f, testCatalog)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Updated, []string{"starter_monthly"}) || len(res.Created) != 0 || len(res.Archived) != 0 {
		t.Errorf("changed metadata value not caught: %+v", res)
	}
}

func TestSeedStopsOnStripeError(t *testing.T) {
	f := newFakeAPI()
	f.fail["CreatePrice"] = &stripe.Error{Msg: "boom", HTTPStatusCode: 500}
	if _, err := Seed(context.Background(), f, testCatalog); err == nil {
		t.Error("error swallowed")
	}
}
