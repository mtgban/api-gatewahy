package billing_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/mtgban/api-gatewahy/billing"
	"github.com/mtgban/api-gatewahy/billing/billingtest"
	"github.com/mtgban/mtgban-website/apiproductlist"
	"github.com/stripe/stripe-go/v84"
)

func TestSeedCreatesEverythingOnce(t *testing.T) {
	f := billingtest.NewFakeAPI()
	ctx := context.Background()
	res, err := billing.Seed(ctx, f, testCatalog)
	if err != nil {
		t.Fatal(err)
	}
	wantItems := len(testCatalog.Packages) + len(testCatalog.Addons)
	wantPrices := wantItems * len(testCatalog.Intervals)
	if len(res.Created) != wantItems+wantPrices || len(res.Updated) != 0 || len(res.Archived) != 0 {
		t.Errorf("first seed: %+v", res)
	}
	if len(f.Products) != wantItems || len(f.Prices) != wantPrices {
		t.Errorf("fake holds %d products %d prices", len(f.Products), len(f.Prices))
	}
	p := f.PriceByKey("starter_quarterly")
	if p == nil || p.UnitAmount != 60000 || p.Product.ID != billing.ProductID("starter") || p.Recurring.Interval != "month" || p.Recurring.IntervalCount != 3 ||
		p.Currency != "usd" || p.Metadata["package"] != "starter" || p.Metadata["store_scope"] != "explicit" || p.Metadata["modes"] != "retail,buylist" || p.Metadata["interval_key"] != "quarterly" {
		t.Errorf("starter_quarterly %+v", p)
	}
	a := f.PriceByKey("extra_game_monthly")
	addon, ok := testCatalog.Addon("extra_game")
	if !ok {
		t.Fatal("no extra_game addon in testCatalog")
	}
	if a == nil || a.UnitAmount != 15000 || a.Metadata["kind"] != "addon" || a.Metadata["addon"] != "extra_game" || a.Metadata["applies_to"] != strings.Join(addon.AppliesTo, ",") {
		t.Errorf("extra_game_monthly %+v", a)
	}
	if prod := f.Products[billing.ProductID("all_data")]; prod == nil || prod.Name != "All Access" || prod.Metadata["kind"] != "package" {
		t.Errorf("all_data product %+v", prod)
	}

	second, err := billing.Seed(ctx, f, testCatalog)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Created)+len(second.Updated)+len(second.Archived) != 0 {
		t.Errorf("second seed changed things: %+v", second)
	}
}

func TestSeedReplacesChangedAmount(t *testing.T) {
	f := billingtest.NewFakeAPI()
	ctx := context.Background()
	if _, err := billing.Seed(ctx, f, testCatalog); err != nil {
		t.Fatal(err)
	}
	old := f.PriceByKey("all_stores_monthly")

	changed := *testCatalog
	changed.Packages = append([]apiproductlist.Package(nil), testCatalog.Packages...)
	for i := range changed.Packages {
		if changed.Packages[i].Key == "all_stores" {
			changed.Packages[i].Monthly = 55000
		}
	}
	res, err := billing.Seed(ctx, f, &changed)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Created, []string{"all_stores_monthly", "all_stores_quarterly"}) || len(res.Archived) != 2 || len(res.Updated) != 0 {
		t.Errorf("replace: %+v", res)
	}
	if old.Active || old.LookupKey != "" {
		t.Errorf("old price still live: %+v", old)
	}
	if now := f.PriceByKey("all_stores_monthly"); now == nil || now.ID == old.ID || now.UnitAmount != 55000 {
		t.Errorf("replacement %+v", now)
	}
}

func TestSeedRefreshesDriftAndReactivates(t *testing.T) {
	f := billingtest.NewFakeAPI()
	ctx := context.Background()
	if _, err := billing.Seed(ctx, f, testCatalog); err != nil {
		t.Fatal(err)
	}
	p := f.PriceByKey("all_data_monthly")
	p.Active = false
	p.Metadata = map[string]string{"stale": "yes"}
	f.Products[billing.ProductID("extra_store")].Active = false
	f.Products[billing.ProductID("starter")].Name = "renamed by hand"

	res, err := billing.Seed(ctx, f, testCatalog)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Updated, []string{billing.ProductID("starter"), billing.ProductID("extra_store"), "all_data_monthly"}) || len(res.Created) != 0 || len(res.Archived) != 0 {
		t.Errorf("refresh: %+v", res)
	}
	if !p.Active || p.Metadata["package"] != "all_data" || p.Metadata["stale"] != "" {
		t.Errorf("price not refreshed: %+v", p)
	}
	if !f.Products[billing.ProductID("extra_store")].Active || f.Products[billing.ProductID("starter")].Name != "À la carte" {
		t.Error("products not refreshed")
	}
}

func TestSeedIgnoresExtraMetadata(t *testing.T) {
	f := billingtest.NewFakeAPI()
	ctx := context.Background()
	if _, err := billing.Seed(ctx, f, testCatalog); err != nil {
		t.Fatal(err)
	}
	p := f.PriceByKey("starter_monthly")
	p.Metadata["note"] = "set by hand"

	res, err := billing.Seed(ctx, f, testCatalog)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created)+len(res.Updated)+len(res.Archived) != 0 {
		t.Errorf("extra metadata key triggered a change: %+v", res)
	}

	p.Metadata["modes"] = "retail"
	res, err = billing.Seed(ctx, f, testCatalog)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Updated, []string{"starter_monthly"}) || len(res.Created) != 0 || len(res.Archived) != 0 {
		t.Errorf("changed metadata value not caught: %+v", res)
	}
}

func TestSeedStopsOnStripeError(t *testing.T) {
	f := billingtest.NewFakeAPI()
	f.Fail["CreatePrice"] = &stripe.Error{Msg: "boom", HTTPStatusCode: 500}
	if _, err := billing.Seed(context.Background(), f, testCatalog); err == nil {
		t.Error("error swallowed")
	}
}

type tier struct{ upTo, unit int64 }

func tiersOf(p *stripe.Price) []tier {
	var out []tier
	for _, t := range p.Tiers {
		out = append(out, tier{t.UpTo, t.UnitAmount})
	}
	return out
}

func TestSeedCreatesTieredPricesForSteppedAddons(t *testing.T) {
	f := billingtest.NewFakeAPI()
	if _, err := billing.Seed(context.Background(), f, steppedCatalog); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string][]tier{
		"extra_store_monthly":   {{1, 15000}, {0, 5000}},
		"extra_store_quarterly": {{1, 45000}, {0, 15000}},
		"extra_game_monthly":    {{1, 25000}, {0, 10000}},
	} {
		p := f.PriceByKey(key)
		if p == nil || p.BillingScheme != stripe.PriceBillingSchemeTiered || p.TiersMode != stripe.PriceTiersModeGraduated || p.UnitAmount != 0 || !slices.Equal(tiersOf(p), want) {
			t.Errorf("%s: %+v tiers %v", key, p, tiersOf(p))
		}
	}
	if p := f.PriceByKey("starter_monthly"); p == nil || p.BillingScheme == stripe.PriceBillingSchemeTiered || len(p.Tiers) != 0 || p.UnitAmount != packageMonthly("starter") {
		t.Errorf("starter_monthly %+v", p)
	}
}

func TestSeedLeavesAMatchingTieredPriceAlone(t *testing.T) {
	f := billingtest.NewFakeAPI()
	ctx := context.Background()
	if _, err := billing.Seed(ctx, f, steppedCatalog); err != nil {
		t.Fatal(err)
	}
	res, err := billing.Seed(ctx, f, steppedCatalog)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created)+len(res.Updated)+len(res.Archived) != 0 {
		t.Errorf("second seed changed things: %+v", res)
	}
}

func TestSeedReplacesAFlatPriceWhenTheAddonBecomesStepped(t *testing.T) {
	f := billingtest.NewFakeAPI()
	ctx := context.Background()
	if _, err := billing.Seed(ctx, f, testCatalog); err != nil {
		t.Fatal(err)
	}
	old := f.PriceByKey("extra_store_monthly")
	res, err := billing.Seed(ctx, f, steppedCatalog)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"extra_store_monthly", "extra_store_quarterly", "extra_game_monthly", "extra_game_quarterly"}
	if !slices.Equal(res.Created, want) || len(res.Archived) != 4 || len(res.Updated) != 0 {
		t.Errorf("replace: %+v", res)
	}
	for _, id := range res.Archived {
		if p := f.Prices[id]; p.Active || p.LookupKey != "" {
			t.Errorf("archived price still live: %+v", p)
		}
	}
	if now := f.PriceByKey("extra_store_monthly"); now == nil || now.ID == old.ID || now.BillingScheme != stripe.PriceBillingSchemeTiered {
		t.Errorf("replacement %+v", now)
	}

	// And back: a flat spec does not accept the tiered price.
	res, err = billing.Seed(ctx, f, testCatalog)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Created, want) || len(res.Archived) != 4 {
		t.Errorf("back to flat: %+v", res)
	}
}

func TestSeedReplacesATieredPriceWhoseStepChanged(t *testing.T) {
	f := billingtest.NewFakeAPI()
	ctx := context.Background()
	if _, err := billing.Seed(ctx, f, steppedCatalog); err != nil {
		t.Fatal(err)
	}
	changed := stepped(steppedCatalog, map[string][2]int64{"extra_store": {16000, 5000}})
	res, err := billing.Seed(ctx, f, changed)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Created, []string{"extra_store_monthly", "extra_store_quarterly"}) || len(res.Archived) != 2 || len(res.Updated) != 0 {
		t.Errorf("replace: %+v", res)
	}
	if p := f.PriceByKey("extra_store_monthly"); p == nil || !slices.Equal(tiersOf(p), []tier{{1, 16000}, {0, 5000}}) {
		t.Errorf("replacement tiers %v", tiersOf(p))
	}
}
