package billing_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/mtgban/api-gatewahy/apiaccess/apiaccesstest"
	"github.com/mtgban/api-gatewahy/billing"
	"github.com/mtgban/api-gatewahy/billing/billingtest"
	"github.com/mtgban/mtgban-website/apiproductlist"
	"github.com/stripe/stripe-go/v84"
)

// testStripe returns a client for STRIPE_TEST_KEY, or skips.
func testStripe(t *testing.T) billing.API {
	t.Helper()
	key := os.Getenv("STRIPE_TEST_KEY")
	if key == "" {
		t.Skip("STRIPE_TEST_KEY not set")
	}
	if !strings.HasPrefix(key, "sk_test_") {
		t.Fatal("STRIPE_TEST_KEY must be a test-mode secret key")
	}
	return billing.NewClient(key)
}

func TestSeedIsIdempotentAgainstStripe(t *testing.T) {
	api := testStripe(t)
	ctx := context.Background()
	if _, err := billing.Seed(ctx, api, testCatalog); err != nil {
		t.Fatal(err)
	}
	second, err := billing.Seed(ctx, api, testCatalog)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(second.Created) + len(second.Updated) + len(second.Archived); n != 0 {
		t.Errorf("second seed changed %d things: %+v", n, second)
	}
	ids, err := billing.PriceIDs(ctx, api)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := billing.SpecLookupKeys(testCatalog)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		if ids[key] == "" {
			t.Errorf("no active price for %s", key)
		}
	}
}

func TestCheckoutSessionPerPackageAgainstStripe(t *testing.T) {
	api := testStripe(t)
	ctx := context.Background()
	if _, err := billing.Seed(ctx, api, testCatalog); err != nil {
		t.Fatal(err)
	}
	cust, err := api.CreateCustomer(ctx, &stripe.CustomerCreateParams{Email: stripe.String("integration@example.com"), Metadata: map[string]string{"account_id": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	store := apiaccesstest.New()
	account, err := store.CreateAccount(ctx, "integration@example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if account.StripeCustomerID, err = store.SetStripeCustomerID(ctx, account.ID, cust.ID); err != nil {
		t.Fatal(err)
	}
	co := &billing.Checkout{
		Store: store, API: api, Catalog: testCatalog, Stores: billingtest.NewFakeStores(), Games: []string{"magic", "pokemon"},
		SuccessURL: "https://api.mtgban.com/checkout/success", CancelURL: "https://api.mtgban.com/checkout/cancel",
	}
	for _, pkg := range testCatalog.Packages {
		plan := billing.Plan{Package: pkg.Key, Interval: "monthly", Games: []string{"magic", "pokemon"}}
		if pkg.StoreScope == apiproductlist.StoreScopeExplicit {
			plan.Stores = []string{"cardkingdom", "starcitygames"}
		}
		sess, err := co.Create(ctx, billing.Request{Account: account, Plan: plan})
		if err != nil {
			t.Errorf("%s: %v", pkg.Key, err)
			continue
		}
		if !strings.HasPrefix(sess.URL, "https://checkout.stripe.com/") {
			t.Errorf("%s: sess.URL %q", pkg.Key, sess.URL)
		}
	}
}
