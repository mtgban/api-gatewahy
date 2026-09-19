package billing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/mtgban-website/apiproductlist"
	"github.com/stripe/stripe-go/v84"
)

// seededFake is a fake Stripe holding every catalog price.
func seededFake(t *testing.T) *fakeAPI {
	t.Helper()
	f := newFakeAPI()
	if _, err := Seed(context.Background(), f, testCatalog); err != nil {
		t.Fatal(err)
	}
	return f
}

var testAccount = apiaccess.Account{ID: 7, Email: "ck@example.com", Status: "active"}

func newTestCheckout(f *fakeAPI, s *memStore) *Checkout {
	return &Checkout{
		Store: s, API: f, Catalog: testCatalog, Games: testGames,
		SuccessURL: "https://api.mtgban.com/checkout/success", CancelURL: "https://api.mtgban.com/checkout/cancel",
		Now: func() time.Time { return time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC) },
	}
}

func TestCheckoutCreatesSessionAndCustomer(t *testing.T) {
	f := seededFake(t)
	s := newMemStore(testAccount)
	co := newTestCheckout(f, s)
	ctx := context.Background()

	url, err := co.Create(ctx, Request{Account: testAccount, Plan: Plan{Package: "starter", Interval: "monthly", Games: []string{"pokemon"}, Stores: []string{"CK", "SCG"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(url, "https://checkout.stripe.test/") {
		t.Errorf("url %q", url)
	}
	if len(f.sessions) != 1 {
		t.Fatalf("sessions %d", len(f.sessions))
	}
	p := f.sessions[0]
	if stripe.StringValue(p.Mode) != "subscription" || !stripe.BoolValue(p.AllowPromotionCodes) ||
		stripe.StringValue(p.ClientReferenceID) != "7" || stripe.StringValue(p.SuccessURL) != co.SuccessURL || stripe.StringValue(p.CancelURL) != co.CancelURL {
		t.Errorf("session params %+v", p)
	}
	if stripe.StringValue(p.Customer) != s.accounts[7].StripeCustomerID || s.accounts[7].StripeCustomerID == "" {
		t.Errorf("customer %q stored %q", stripe.StringValue(p.Customer), s.accounts[7].StripeCustomerID)
	}
	if cust := f.customers[s.accounts[7].StripeCustomerID]; cust == nil || cust.Email != "ck@example.com" || cust.Metadata["account_id"] != "7" {
		t.Errorf("customer %+v", cust)
	}
	want := map[string]int64{f.priceByKey("starter_monthly").ID: 1, f.priceByKey("extra_store_monthly").ID: 1, f.priceByKey("extra_game_monthly").ID: 1}
	if len(p.LineItems) != 3 {
		t.Fatalf("line items %d", len(p.LineItems))
	}
	for _, li := range p.LineItems {
		if want[stripe.StringValue(li.Price)] != stripe.Int64Value(li.Quantity) {
			t.Errorf("line item %s x%d", stripe.StringValue(li.Price), stripe.Int64Value(li.Quantity))
		}
	}
	md := p.SubscriptionData.Metadata
	if md["package"] != "starter" || md["interval"] != "monthly" || md["games"] != "magic,pokemon" || md["stores"] != "CK,SCG" || md["account_id"] != "7" {
		t.Errorf("metadata %v", md)
	}

	// A second checkout reuses the stored customer.
	if _, err := co.Create(ctx, Request{Account: s.accounts[7], Plan: Plan{Package: "all_data", Interval: "monthly"}}); err != nil {
		t.Fatal(err)
	}
	if f.calls["CreateCustomer"] != 1 {
		t.Errorf("CreateCustomer called %d times", f.calls["CreateCustomer"])
	}
}

func TestCheckoutRejectsBadPlans(t *testing.T) {
	f := seededFake(t)
	co := newTestCheckout(f, newMemStore(testAccount))
	ctx := context.Background()
	if _, err := co.Create(ctx, Request{Account: testAccount, Plan: Plan{Package: "all_data", Interval: "quarterly"}}); !errors.Is(err, ErrInviteRequired) {
		t.Errorf("quarterly without invite: %v", err)
	}
	if _, err := co.Create(ctx, Request{Account: testAccount, Plan: Plan{Package: "starter", Interval: "monthly"}}); err == nil {
		t.Error("starter without stores accepted")
	}
	if _, err := co.Create(ctx, Request{Account: testAccount, Plan: Plan{Package: "all_data", Interval: "monthly", Games: []string{"yugioh"}}}); err == nil {
		t.Error("unknown game accepted")
	}
	suspended := testAccount
	suspended.Status = "suspended"
	if _, err := co.Create(ctx, Request{Account: suspended, Plan: Plan{Package: "all_data", Interval: "monthly"}}); err == nil || !strings.Contains(err.Error(), "suspended") {
		t.Errorf("suspended account: %v", err)
	}
	if len(f.sessions) != 0 || f.calls["CreateCustomer"] != 0 {
		t.Error("a rejected plan reached Stripe")
	}
}

func TestCheckoutInvites(t *testing.T) {
	f := seededFake(t)
	s := newMemStore(testAccount)
	co := newTestCheckout(f, s)
	ctx := context.Background()
	later := co.Now().Add(24 * time.Hour)
	quarterly := Plan{Package: "all_stores", Interval: "quarterly"}

	if _, err := co.Create(ctx, Request{Account: testAccount, Plan: quarterly, Invite: "nope"}); !errors.Is(err, apiaccess.ErrInviteInvalid) {
		t.Errorf("unknown invite: %v", err)
	}

	s.addInvite("bound", "quarterly", "someone@else.com", later)
	if _, err := co.Create(ctx, Request{Account: testAccount, Plan: quarterly, Invite: "bound"}); !errors.Is(err, apiaccess.ErrInviteInvalid) {
		t.Errorf("invite bound to another email: %v", err)
	}

	s.addInvite("wrong-interval", "annual", "", later)
	if _, err := co.Create(ctx, Request{Account: testAccount, Plan: quarterly, Invite: "wrong-interval"}); err == nil || !strings.Contains(err.Error(), "annual") {
		t.Errorf("invite for another interval: %v", err)
	}
	if s.invites["wrong-interval"].UsedAt != nil {
		t.Error("mismatched invite was not released")
	}

	s.addInvite("good", "quarterly", "CK@example.com", later)
	f.fail["CreateCheckoutSession"] = errors.New("stripe down")
	if _, err := co.Create(ctx, Request{Account: testAccount, Plan: quarterly, Invite: "good"}); err == nil {
		t.Error("stripe failure swallowed")
	}
	if s.invites["good"].UsedAt != nil {
		t.Error("invite not released after a failed session")
	}
	delete(f.fail, "CreateCheckoutSession")
	if _, err := co.Create(ctx, Request{Account: s.accounts[7], Plan: quarterly, Invite: "good"}); err != nil {
		t.Fatalf("retry with released invite: %v", err)
	}
	if s.invites["good"].UsedAt == nil {
		t.Error("invite not consumed")
	}
	if _, err := co.Create(ctx, Request{Account: s.accounts[7], Plan: quarterly, Invite: "good"}); !errors.Is(err, apiaccess.ErrInviteInvalid) {
		t.Errorf("invite replayed: %v", err)
	}
	if len(f.sessions) != 1 || stripe.StringValue(f.sessions[0].LineItems[0].Price) != f.priceByKey("all_stores_quarterly").ID {
		t.Errorf("sessions %+v", f.sessions)
	}
}

func TestCheckoutNeedsSeededPrices(t *testing.T) {
	f := newFakeAPI()
	co := newTestCheckout(f, newMemStore(testAccount))
	_, err := co.Create(context.Background(), Request{Account: testAccount, Plan: Plan{Package: "all_data", Interval: "monthly"}})
	if !errors.Is(err, ErrPriceNotSeeded) {
		t.Errorf("unseeded: %v", err)
	}
	if f.calls["CreateCustomer"] != 0 {
		t.Error("customer created before the line items were resolved")
	}
}

func TestCheckoutEveryPackage(t *testing.T) {
	f := seededFake(t)
	co := newTestCheckout(f, newMemStore(testAccount))
	for _, pkg := range testCatalog.Packages {
		plan := Plan{Package: pkg.Key, Interval: "monthly"}
		if pkg.StoreScope == apiproductlist.StoreScopeExplicit {
			plan.Stores = []string{"CK"}
		}
		if _, err := co.Create(context.Background(), Request{Account: testAccount, Plan: plan}); err != nil {
			t.Errorf("%s: %v", pkg.Key, err)
		}
	}
}
