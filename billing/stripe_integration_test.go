package billing

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/mtgban-website/apiproductlist"
	"github.com/stripe/stripe-go/v84"
)

// testStripe returns a client for STRIPE_TEST_KEY, or skips.
func testStripe(t *testing.T) API {
	t.Helper()
	key := os.Getenv("STRIPE_TEST_KEY")
	if key == "" {
		t.Skip("STRIPE_TEST_KEY not set")
	}
	if !strings.HasPrefix(key, "sk_test_") {
		t.Fatal("STRIPE_TEST_KEY must be a test-mode secret key")
	}
	return NewClient(key)
}

func TestSeedIsIdempotentAgainstStripe(t *testing.T) {
	api := testStripe(t)
	ctx := context.Background()
	if _, err := Seed(ctx, api, testCatalog); err != nil {
		t.Fatal(err)
	}
	second, err := Seed(ctx, api, testCatalog)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(second.Created) + len(second.Updated) + len(second.Archived); n != 0 {
		t.Errorf("second seed changed %d things: %+v", n, second)
	}
	ids, err := priceIDs(ctx, api)
	if err != nil {
		t.Fatal(err)
	}
	specs, err := priceSpecs(testCatalog)
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range specs {
		if ids[spec.lookupKey] == "" {
			t.Errorf("no active price for %s", spec.lookupKey)
		}
	}
}

func TestCheckoutSessionPerPackageAgainstStripe(t *testing.T) {
	api := testStripe(t)
	ctx := context.Background()
	if _, err := Seed(ctx, api, testCatalog); err != nil {
		t.Fatal(err)
	}
	cust, err := api.CreateCustomer(ctx, &stripe.CustomerCreateParams{Email: stripe.String("integration@example.com"), Metadata: map[string]string{"account_id": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	account := apiaccess.Account{ID: 1, Email: "integration@example.com", Status: "active", StripeCustomerID: cust.ID}
	co := &Checkout{
		Store: newMemStore(account), API: api, Catalog: testCatalog, Games: []string{"magic", "pokemon"},
		SuccessURL: "https://api.mtgban.com/checkout/success", CancelURL: "https://api.mtgban.com/checkout/cancel",
	}
	for _, pkg := range testCatalog.Packages {
		plan := Plan{Package: pkg.Key, Interval: "monthly", Games: []string{"pokemon"}}
		if pkg.StoreScope == apiproductlist.StoreScopeExplicit {
			plan.Stores = []string{"CK", "SCG"}
		}
		url, err := co.Create(ctx, Request{Account: account, Plan: plan})
		if err != nil {
			t.Errorf("%s: %v", pkg.Key, err)
			continue
		}
		if !strings.HasPrefix(url, "https://checkout.stripe.com/") {
			t.Errorf("%s: url %q", pkg.Key, url)
		}
	}
}
