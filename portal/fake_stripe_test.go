package portal

import (
	"context"
	"errors"
	"fmt"

	"github.com/mtgban/api-gatewahy/billing"
	"github.com/mtgban/mtgban-website/apiproductlist"
	"github.com/stripe/stripe-go/v84"
)

// fakeStripe answers the few calls the portal makes; anything else fails loudly.
type fakeStripe struct {
	billing.API
	prices    []*stripe.Price
	sub       *stripe.Subscription
	updated   *stripe.SubscriptionUpdateParams
	checkouts int
	sessions  map[string]stripe.CheckoutSessionStatus
	fail      error
}

// seededPrices is one active price per catalog item and interval, as Seed would leave them.
func seededPrices(cat *apiproductlist.ProductList) []*stripe.Price {
	var out []*stripe.Price
	add := func(key string) {
		for _, iv := range cat.Intervals {
			lk := apiproductlist.LookupKey(key, iv.Key)
			out = append(out, &stripe.Price{ID: "price_" + lk, LookupKey: lk, Active: true})
		}
	}
	for _, p := range cat.Packages {
		add(p.Key)
	}
	for _, a := range cat.Addons {
		add(a.Key)
	}
	return out
}

func (f *fakeStripe) ListPrices(context.Context) ([]*stripe.Price, error) { return f.prices, nil }

func (f *fakeStripe) CreateCustomer(_ context.Context, p *stripe.CustomerCreateParams) (*stripe.Customer, error) {
	return &stripe.Customer{ID: "cus_test", Email: *p.Email}, nil
}

func (f *fakeStripe) CreateCheckoutSession(context.Context, *stripe.CheckoutSessionCreateParams) (*stripe.CheckoutSession, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	f.checkouts++
	id := fmt.Sprintf("cs_test_%d", f.checkouts)
	if f.sessions == nil {
		f.sessions = map[string]stripe.CheckoutSessionStatus{}
	}
	f.sessions[id] = stripe.CheckoutSessionStatusOpen
	return &stripe.CheckoutSession{ID: id, URL: "https://checkout.stripe.com/c/pay/" + id}, nil
}

// ExpireCheckoutSession expires an open session; a completed one is refused like Stripe does.
func (f *fakeStripe) ExpireCheckoutSession(_ context.Context, id string) (*stripe.CheckoutSession, error) {
	if f.sessions[id] != stripe.CheckoutSessionStatusOpen {
		return nil, fmt.Errorf("fake stripe: session %s is not open", id)
	}
	f.sessions[id] = stripe.CheckoutSessionStatusExpired
	return &stripe.CheckoutSession{ID: id, Status: stripe.CheckoutSessionStatusExpired}, nil
}

func (f *fakeStripe) CreatePortalSession(context.Context, *stripe.BillingPortalSessionCreateParams) (*stripe.BillingPortalSession, error) {
	return &stripe.BillingPortalSession{URL: "https://billing.stripe.com/p/session/test"}, nil
}

func (f *fakeStripe) GetSubscription(_ context.Context, id string) (*stripe.Subscription, error) {
	if f.sub == nil || f.sub.ID != id {
		return nil, errors.New("no such subscription")
	}
	return f.sub, nil
}

func (f *fakeStripe) UpdateSubscription(_ context.Context, _ string, p *stripe.SubscriptionUpdateParams) (*stripe.Subscription, error) {
	f.updated = p
	return f.sub, nil
}

// withStripe turns billing on for a test server.
func (ts *testServer) withStripe() *fakeStripe {
	f := &fakeStripe{prices: seededPrices(ts.Catalog)}
	ts.Stripe = f
	ts.Checkout = &billing.Checkout{Store: ts.store, API: f, Catalog: ts.Catalog, Stores: ts.Stores, Games: ts.Games,
		SuccessURL: ts.PublicURL + ts.SuccessPath, CancelURL: ts.PublicURL + ts.CancelPath, Now: ts.Now}
	ts.Reconcile = func(context.Context, string) error { return nil }
	return f
}

// fakeStores is a StoreLister over two fixed game sites; fail makes every call error.
type fakeStores struct {
	sites map[string]billing.SiteStores
	fail  error
	down  map[string]bool
	calls map[string]int
}

func newFakeStores() *fakeStores {
	tcg := billing.StoreFamily{Key: "tcgplayer", Name: "TCGplayer", Shorthands: []string{"TCGLow", "TCGMarket", "TCGDirect", "TCGDirectNet", "TCGPlayer"}}
	return &fakeStores{sites: map[string]billing.SiteStores{
		"magic": {Game: "magic", Implied: []billing.StoreFamily{tcg}, Stores: []billing.StoreFamily{
			{Key: "cardkingdom", Name: "Card Kingdom", Shorthands: []string{"CK"}},
			{Key: "starcitygames", Name: "Star City Games", Shorthands: []string{"SCG"}},
		}},
		"pokemon": {Game: "pokemon", Implied: []billing.StoreFamily{tcg}, Stores: []billing.StoreFamily{
			{Key: "cardkingdom", Name: "Card Kingdom", Shorthands: []string{"CK", "CKBLLast"}},
			{Key: "trollandtoad", Name: "Troll and Toad", Shorthands: []string{"TNT"}},
		}},
	}}
}

func (f *fakeStores) SiteStores(_ context.Context, game string) (billing.SiteStores, error) {
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[game]++
	if f.down[game] {
		return billing.SiteStores{}, fmt.Errorf("fake stores: %s is down", game)
	}
	if f.fail != nil {
		return billing.SiteStores{}, f.fail
	}
	site, ok := f.sites[game]
	if !ok {
		return billing.SiteStores{}, fmt.Errorf("fake stores: no site for %s", game)
	}
	return site, nil
}
