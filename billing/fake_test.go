package billing

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v84"
)

// fakeAPI is an in-memory Stripe with just enough state for the billing tests.
type fakeAPI struct {
	seq           int
	customers     map[string]*stripe.Customer
	products      map[string]*stripe.Product
	prices        map[string]*stripe.Price
	subs          map[string]*stripe.Subscription
	sessions      []*stripe.CheckoutSessionCreateParams
	sessionStatus map[string]stripe.CheckoutSessionStatus
	portals       []*stripe.BillingPortalSessionCreateParams
	updates       map[string][]*stripe.SubscriptionUpdateParams
	fail          map[string]error
	calls         map[string]int
}

var _ API = (*fakeAPI)(nil)

func newFakeAPI() *fakeAPI {
	return &fakeAPI{
		customers: map[string]*stripe.Customer{},
		products:  map[string]*stripe.Product{},
		prices:    map[string]*stripe.Price{},
		subs:      map[string]*stripe.Subscription{},
		updates:   map[string][]*stripe.SubscriptionUpdateParams{},
		fail:      map[string]error{},
		calls:     map[string]int{},
	}
}

func missing(what string) error {
	return &stripe.Error{Code: stripe.ErrorCodeResourceMissing, Msg: "no such " + what, HTTPStatusCode: 404}
}

// enter counts the call and returns the injected failure, if any.
func (f *fakeAPI) enter(method string) error {
	f.calls[method]++
	return f.fail[method]
}

func (f *fakeAPI) next(prefix string) string {
	f.seq++
	return fmt.Sprintf("%s_%d", prefix, f.seq)
}

func (f *fakeAPI) CreateCustomer(_ context.Context, p *stripe.CustomerCreateParams) (*stripe.Customer, error) {
	if err := f.enter("CreateCustomer"); err != nil {
		return nil, err
	}
	c := &stripe.Customer{ID: f.next("cus"), Email: stripe.StringValue(p.Email), Metadata: p.Metadata}
	f.customers[c.ID] = c
	return c, nil
}

func (f *fakeAPI) CreateCheckoutSession(_ context.Context, p *stripe.CheckoutSessionCreateParams) (*stripe.CheckoutSession, error) {
	if err := f.enter("CreateCheckoutSession"); err != nil {
		return nil, err
	}
	f.sessions = append(f.sessions, p)
	id := f.next("cs")
	if f.sessionStatus == nil {
		f.sessionStatus = map[string]stripe.CheckoutSessionStatus{}
	}
	f.sessionStatus[id] = stripe.CheckoutSessionStatusOpen
	return &stripe.CheckoutSession{ID: id, URL: "https://checkout.stripe.test/" + id}, nil
}

// ExpireCheckoutSession expires an open session; Stripe refuses once it is complete or gone.
func (f *fakeAPI) ExpireCheckoutSession(_ context.Context, id string) (*stripe.CheckoutSession, error) {
	if err := f.enter("ExpireCheckoutSession"); err != nil {
		return nil, err
	}
	if f.sessionStatus[id] != stripe.CheckoutSessionStatusOpen {
		return nil, fmt.Errorf("fake stripe: session %s is not open", id)
	}
	f.sessionStatus[id] = stripe.CheckoutSessionStatusExpired
	return &stripe.CheckoutSession{ID: id, Status: stripe.CheckoutSessionStatusExpired}, nil
}

func (f *fakeAPI) GetSubscription(_ context.Context, id string) (*stripe.Subscription, error) {
	if err := f.enter("GetSubscription"); err != nil {
		return nil, err
	}
	s, ok := f.subs[id]
	if !ok {
		return nil, missing("subscription")
	}
	return s, nil
}

func (f *fakeAPI) ListSubscriptions(context.Context) ([]*stripe.Subscription, error) {
	if err := f.enter("ListSubscriptions"); err != nil {
		return nil, err
	}
	var out []*stripe.Subscription
	for _, id := range sortedKeys(f.subs) {
		if s := f.subs[id]; s.Status != stripe.SubscriptionStatusCanceled {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeAPI) UpdateSubscription(_ context.Context, id string, p *stripe.SubscriptionUpdateParams) (*stripe.Subscription, error) {
	if err := f.enter("UpdateSubscription"); err != nil {
		return nil, err
	}
	s, ok := f.subs[id]
	if !ok {
		return nil, missing("subscription")
	}
	f.updates[id] = append(f.updates[id], p)
	if p.Metadata != nil {
		s.Metadata = p.Metadata
	}
	return s, nil
}

func (f *fakeAPI) GetProduct(_ context.Context, id string) (*stripe.Product, error) {
	if err := f.enter("GetProduct"); err != nil {
		return nil, err
	}
	p, ok := f.products[id]
	if !ok {
		return nil, missing("product")
	}
	return p, nil
}

func (f *fakeAPI) CreateProduct(_ context.Context, p *stripe.ProductCreateParams) (*stripe.Product, error) {
	if err := f.enter("CreateProduct"); err != nil {
		return nil, err
	}
	prod := &stripe.Product{ID: stripe.StringValue(p.ID), Name: stripe.StringValue(p.Name), Active: true, Metadata: p.Metadata}
	if prod.ID == "" {
		prod.ID = f.next("prod")
	}
	f.products[prod.ID] = prod
	return prod, nil
}

func (f *fakeAPI) UpdateProduct(_ context.Context, id string, p *stripe.ProductUpdateParams) (*stripe.Product, error) {
	if err := f.enter("UpdateProduct"); err != nil {
		return nil, err
	}
	prod, ok := f.products[id]
	if !ok {
		return nil, missing("product")
	}
	if p.Name != nil {
		prod.Name = *p.Name
	}
	if p.Active != nil {
		prod.Active = *p.Active
	}
	if p.Metadata != nil {
		prod.Metadata = p.Metadata
	}
	return prod, nil
}

func (f *fakeAPI) ListPrices(context.Context) ([]*stripe.Price, error) {
	if err := f.enter("ListPrices"); err != nil {
		return nil, err
	}
	var out []*stripe.Price
	for _, id := range sortedKeys(f.prices) {
		out = append(out, f.prices[id])
	}
	return out, nil
}

func (f *fakeAPI) CreatePrice(_ context.Context, p *stripe.PriceCreateParams) (*stripe.Price, error) {
	if err := f.enter("CreatePrice"); err != nil {
		return nil, err
	}
	key := stripe.StringValue(p.LookupKey)
	for _, other := range f.prices {
		if key != "" && other.LookupKey == key {
			if !stripe.BoolValue(p.TransferLookupKey) {
				return nil, &stripe.Error{Msg: "lookup_key already in use", HTTPStatusCode: 400}
			}
			other.LookupKey = ""
		}
	}
	price := &stripe.Price{
		ID: f.next("price"), Active: true, LookupKey: key, Metadata: p.Metadata,
		UnitAmount: stripe.Int64Value(p.UnitAmount), Currency: stripe.Currency(stripe.StringValue(p.Currency)),
		Product: &stripe.Product{ID: stripe.StringValue(p.Product)},
	}
	if p.Recurring != nil {
		price.Recurring = &stripe.PriceRecurring{
			Interval:      stripe.PriceRecurringInterval(stripe.StringValue(p.Recurring.Interval)),
			IntervalCount: stripe.Int64Value(p.Recurring.IntervalCount),
		}
	}
	f.prices[price.ID] = price
	return price, nil
}

func (f *fakeAPI) UpdatePrice(_ context.Context, id string, p *stripe.PriceUpdateParams) (*stripe.Price, error) {
	if err := f.enter("UpdatePrice"); err != nil {
		return nil, err
	}
	price, ok := f.prices[id]
	if !ok {
		return nil, missing("price")
	}
	if p.Active != nil {
		price.Active = *p.Active
	}
	if p.Metadata != nil {
		price.Metadata = p.Metadata
	}
	if p.LookupKey != nil {
		price.LookupKey = *p.LookupKey
	}
	return price, nil
}

func (f *fakeAPI) CreatePortalSession(_ context.Context, p *stripe.BillingPortalSessionCreateParams) (*stripe.BillingPortalSession, error) {
	if err := f.enter("CreatePortalSession"); err != nil {
		return nil, err
	}
	f.portals = append(f.portals, p)
	return &stripe.BillingPortalSession{ID: f.next("bps"), URL: "https://billing.stripe.test/" + stripe.StringValue(p.Customer)}, nil
}

// priceByKey finds the price carrying a lookup key, or nil.
func (f *fakeAPI) priceByKey(key string) *stripe.Price {
	for _, id := range sortedKeys(f.prices) {
		if p := f.prices[id]; p.LookupKey == key {
			return p
		}
	}
	return nil
}

type fakeItem struct {
	key string
	qty int64
}

// addSub installs a subscription whose items reference prices by lookup key.
func (f *fakeAPI) addSub(t *testing.T, id, customerID string, status stripe.SubscriptionStatus, metadata map[string]string, periodEnd time.Time, items ...fakeItem) *stripe.Subscription {
	t.Helper()
	s := &stripe.Subscription{ID: id, Status: status, Metadata: metadata, Customer: &stripe.Customer{ID: customerID}, Items: &stripe.SubscriptionItemList{}}
	for i, it := range items {
		p := f.priceByKey(it.key)
		if p == nil {
			t.Fatalf("no price for %s; seed the fake first", it.key)
		}
		s.Items.Data = append(s.Items.Data, &stripe.SubscriptionItem{
			ID: fmt.Sprintf("si_%s_%d", id, i), Price: p, Quantity: it.qty, CurrentPeriodEnd: periodEnd.Unix(),
		})
	}
	f.subs[id] = s
	return s
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
