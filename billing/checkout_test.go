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
		Store: s, API: f, Catalog: testCatalog, Stores: newFakeStores(), Games: testGames,
		SuccessURL: "https://api.mtgban.com/checkout/success", CancelURL: "https://api.mtgban.com/checkout/cancel",
		Now: func() time.Time { return time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC) },
	}
}

func TestCheckoutCreatesSessionAndCustomer(t *testing.T) {
	f := seededFake(t)
	s := newMemStore(testAccount)
	co := newTestCheckout(f, s)
	ctx := context.Background()

	sess, err := co.Create(ctx, Request{Account: testAccount, Plan: Plan{Package: "starter", Interval: "monthly", Games: []string{"magic", "pokemon"}, Stores: []string{"cardkingdom", "starcitygames"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sess.URL, "https://checkout.stripe.test/") || sess.ID == "" {
		t.Errorf("session %+v", sess)
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
	if md["package"] != "starter" || md["interval"] != "monthly" || md["games"] != "magic,pokemon" || md["stores"] != "cardkingdom,starcitygames" || md["account_id"] != "7" {
		t.Errorf("metadata %v", md)
	}

	// A second checkout reuses the stored customer.
	if _, err := co.Create(ctx, Request{Account: s.accounts[7], Plan: Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}}); err != nil {
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
	if _, err := co.Create(ctx, Request{Account: testAccount, Plan: Plan{Package: "all_data", Interval: "quarterly", Games: []string{"magic"}}}); !errors.Is(err, ErrInviteRequired) {
		t.Errorf("quarterly without invite: %v", err)
	}
	if _, err := co.Create(ctx, Request{Account: testAccount, Plan: Plan{Package: "starter", Interval: "monthly", Games: []string{"magic"}}}); err == nil {
		t.Error("starter without stores accepted")
	}
	if _, err := co.Create(ctx, Request{Account: testAccount, Plan: Plan{Package: "all_data", Interval: "monthly", Games: []string{"yugioh"}}}); err == nil {
		t.Error("unknown game accepted")
	}
	suspended := testAccount
	suspended.Status = "suspended"
	if _, err := co.Create(ctx, Request{Account: suspended, Plan: Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}}); err == nil || !strings.Contains(err.Error(), "suspended") {
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
	quarterly := Plan{Package: "all_stores", Interval: "quarterly", Games: []string{"magic"}}

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
	_, err := co.Create(context.Background(), Request{Account: testAccount, Plan: Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}})
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
		plan := Plan{Package: pkg.Key, Interval: "monthly", Games: []string{"magic"}}
		if pkg.StoreScope == apiproductlist.StoreScopeExplicit {
			plan.Stores = []string{"cardkingdom"}
		}
		if _, err := co.Create(context.Background(), Request{Account: testAccount, Plan: plan}); err != nil {
			t.Errorf("%s: %v", pkg.Key, err)
		}
	}
}

func TestAbandonReleasesInviteOnlyWhenStripeExpiresTheSession(t *testing.T) {
	f := seededFake(t)
	s := newMemStore(testAccount)
	co := newTestCheckout(f, s)
	ctx := context.Background()
	later := co.Now().Add(24 * time.Hour)
	quarterly := Plan{Package: "all_stores", Interval: "quarterly", Games: []string{"magic"}}

	s.addInvite("walk-away", "quarterly", "", later)
	sess, err := co.Create(ctx, Request{Account: testAccount, Plan: quarterly, Invite: "walk-away"})
	if err != nil {
		t.Fatal(err)
	}
	if err := co.Abandon(ctx, sess.ID, "walk-away"); err != nil {
		t.Fatalf("abandon: %v", err)
	}
	if f.sessionStatus[sess.ID] != stripe.CheckoutSessionStatusExpired {
		t.Errorf("session %s is %s", sess.ID, f.sessionStatus[sess.ID])
	}
	if s.invites["walk-away"].UsedAt != nil {
		t.Error("invite not released after the session expired")
	}

	// A session the customer already paid for cannot be expired, so its invite stays spent.
	s.addInvite("paid", "quarterly", "", later)
	sess, err = co.Create(ctx, Request{Account: testAccount, Plan: quarterly, Invite: "paid"})
	if err != nil {
		t.Fatal(err)
	}
	f.sessionStatus[sess.ID] = stripe.CheckoutSessionStatusComplete
	if err := co.Abandon(ctx, sess.ID, "paid"); err == nil {
		t.Error("abandon of a completed session succeeded")
	}
	if s.invites["paid"].UsedAt == nil {
		t.Error("invite released although the session completed")
	}

	// Stripe down: the invite stays spent rather than risk a double spend.
	s.addInvite("outage", "quarterly", "", later)
	sess, err = co.Create(ctx, Request{Account: testAccount, Plan: quarterly, Invite: "outage"})
	if err != nil {
		t.Fatal(err)
	}
	f.fail["ExpireCheckoutSession"] = errors.New("stripe down")
	if err := co.Abandon(ctx, sess.ID, "outage"); err == nil {
		t.Error("stripe failure swallowed")
	}
	if s.invites["outage"].UsedAt == nil {
		t.Error("invite released although Stripe did not confirm the expiry")
	}
}

func TestCheckoutResolvesStoresBeforeStripe(t *testing.T) {
	f := seededFake(t)
	s := newMemStore(testAccount)
	co := newTestCheckout(f, s)
	lister := newFakeStores()
	co.Stores = lister
	ctx := context.Background()
	s.addInvite("held", "quarterly", "", co.Now().Add(time.Hour))

	unknown := Plan{Package: "starter", Interval: "monthly", Games: []string{"magic"}, Stores: []string{"trollandtoad"}}
	var ve *ValidationError
	if _, err := co.Create(ctx, Request{Account: testAccount, Plan: unknown}); !errors.As(err, &ve) || ve.Msg != "Store trollandtoad is not available for the games you picked." {
		t.Errorf("unknown key: %v", err)
	}
	lister.fail = errors.New("connection refused")
	down := Plan{Package: "starter", Interval: "quarterly", Games: []string{"magic"}, Stores: []string{"cardkingdom"}}
	if _, err := co.Create(ctx, Request{Account: testAccount, Plan: down, Invite: "held"}); !errors.Is(err, ErrStoresUnavailable) {
		t.Errorf("site down: %v", err)
	}
	if f.calls["CreateCustomer"] != 0 || f.calls["CreateCheckoutSession"] != 0 || s.invites["held"].UsedAt != nil {
		t.Errorf("stripe or the invite was touched: %v used %v", f.calls, s.invites["held"].UsedAt)
	}
}

func TestCheckoutWithOneSiteDown(t *testing.T) {
	f := seededFake(t)
	co := newTestCheckout(f, newMemStore(testAccount))
	lister := newFakeStores()
	lister.down = map[string]bool{"pokemon": true}
	co.Stores = lister
	plan := Plan{Package: "starter", Interval: "monthly", Games: []string{"magic", "pokemon"}, Stores: []string{"cardkingdom"}}
	if _, err := co.Create(context.Background(), Request{Account: testAccount, Plan: plan}); !errors.Is(err, ErrStoresUnavailable) {
		t.Errorf("pokemon down: %v", err)
	}
	if f.calls["CreateCheckoutSession"] != 0 || f.calls["CreateCustomer"] != 0 {
		t.Errorf("stripe touched: %v", f.calls)
	}
}

func TestCheckoutReusesTheCallersResolve(t *testing.T) {
	f := seededFake(t)
	co := newTestCheckout(f, newMemStore(testAccount))
	lister := newFakeStores()
	co.Stores = lister
	plan, _ := Plan{Package: "starter", Interval: "monthly", Games: []string{"magic"}, Stores: []string{"cardkingdom"}}.Validate(testCatalog, testGames, false)
	resolved, err := plan.Resolve(context.Background(), testCatalog, lister)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := co.Create(context.Background(), Request{Account: testAccount, Plan: plan, Resolved: &resolved}); err != nil {
		t.Fatal(err)
	}
	if lister.calls != 1 {
		t.Errorf("%d site lookups, want the caller's one", lister.calls)
	}
}
