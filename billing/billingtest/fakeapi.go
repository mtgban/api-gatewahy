// Package billingtest holds the billing fakes the billing, portal and root
// tests share: an in-memory Stripe and a fixed list of game sites' stores.
package billingtest

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/mtgban/api-gatewahy/billing"
	"github.com/mtgban/mtgban-website/apiproductlist"
	"github.com/stripe/stripe-go/v84"
)

// FakeAPI is an in-memory Stripe with just enough state for the tests.
// Tests read its fields once the calls under test have returned.
type FakeAPI struct {
	Customers map[string]*stripe.Customer
	Products  map[string]*stripe.Product
	Prices    map[string]*stripe.Price
	Subs      map[string]*stripe.Subscription
	// Sessions are the checkout params in call order; SessionIDs[i] is the id Sessions[i] got.
	Sessions      []*stripe.CheckoutSessionCreateParams
	SessionIDs    []string
	SessionStatus map[string]stripe.CheckoutSessionStatus
	// SessionCustomer is the customer each session id was opened for.
	SessionCustomer map[string]string
	Portals         []*stripe.BillingPortalSessionCreateParams
	Updates         map[string][]*stripe.SubscriptionUpdateParams
	// Fail maps a method name to the error it returns on entry.
	Fail map[string]error
	// Calls counts entries per method name, failed ones included.
	Calls map[string]int

	mu  sync.Mutex
	seq int
}

var _ billing.API = (*FakeAPI)(nil)

// NewFakeAPI returns a Stripe with nothing in it.
func NewFakeAPI() *FakeAPI {
	return &FakeAPI{
		Customers:       map[string]*stripe.Customer{},
		Products:        map[string]*stripe.Product{},
		Prices:          map[string]*stripe.Price{},
		Subs:            map[string]*stripe.Subscription{},
		SessionStatus:   map[string]stripe.CheckoutSessionStatus{},
		SessionCustomer: map[string]string{},
		Updates:         map[string][]*stripe.SubscriptionUpdateParams{},
		Fail:            map[string]error{},
		Calls:           map[string]int{},
	}
}

// SeededFakeAPI is a Stripe holding every price of cat, as billing.Seed leaves it.
func SeededFakeAPI(t testing.TB, cat *apiproductlist.ProductList) *FakeAPI {
	t.Helper()
	f := NewFakeAPI()
	if _, err := billing.Seed(context.Background(), f, cat); err != nil {
		t.Fatal(err)
	}
	return f
}

func missing(what string) error {
	return &stripe.Error{Code: stripe.ErrorCodeResourceMissing, Msg: "no such " + what, HTTPStatusCode: 404}
}

// enter takes the lock, counts the call and returns the injected failure; callers unlock.
func (f *FakeAPI) enter(method string) error {
	f.mu.Lock()
	f.Calls[method]++
	return f.Fail[method]
}

func (f *FakeAPI) next(prefix string) string {
	f.seq++
	return fmt.Sprintf("%s_%d", prefix, f.seq)
}

// CreateCustomer implements billing.API.
func (f *FakeAPI) CreateCustomer(_ context.Context, p *stripe.CustomerCreateParams) (*stripe.Customer, error) {
	err := f.enter("CreateCustomer")
	defer f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	c := &stripe.Customer{ID: f.next("cus"), Email: stripe.StringValue(p.Email), Metadata: p.Metadata}
	f.Customers[c.ID] = c
	return c, nil
}

// CreateCheckoutSession opens a session at a fake checkout URL.
func (f *FakeAPI) CreateCheckoutSession(_ context.Context, p *stripe.CheckoutSessionCreateParams) (*stripe.CheckoutSession, error) {
	err := f.enter("CreateCheckoutSession")
	defer f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	id := f.next("cs")
	f.Sessions = append(f.Sessions, p)
	f.SessionIDs = append(f.SessionIDs, id)
	f.SessionStatus[id] = stripe.CheckoutSessionStatusOpen
	f.SessionCustomer[id] = stripe.StringValue(p.Customer)
	return &stripe.CheckoutSession{ID: id, URL: CheckoutURL(id)}, nil
}

// ListOpenCheckoutSessions returns the customer's open sessions in the order they were opened.
func (f *FakeAPI) ListOpenCheckoutSessions(_ context.Context, customerID string) ([]*stripe.CheckoutSession, error) {
	err := f.enter("ListOpenCheckoutSessions")
	defer f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var out []*stripe.CheckoutSession
	for _, id := range f.SessionIDs {
		if f.SessionCustomer[id] == customerID && f.SessionStatus[id] == stripe.CheckoutSessionStatusOpen {
			out = append(out, &stripe.CheckoutSession{ID: id, Status: stripe.CheckoutSessionStatusOpen})
		}
	}
	return out, nil
}

// CheckoutURL is where the fake sends a customer to pay for session id.
func CheckoutURL(id string) string {
	return "https://checkout.stripe.test/" + id
}

// ExpireCheckoutSession expires an open session; Stripe refuses once it is complete or gone.
func (f *FakeAPI) ExpireCheckoutSession(_ context.Context, id string) (*stripe.CheckoutSession, error) {
	err := f.enter("ExpireCheckoutSession")
	defer f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if f.SessionStatus[id] != stripe.CheckoutSessionStatusOpen {
		return nil, fmt.Errorf("fake stripe: session %s is not open", id)
	}
	f.SessionStatus[id] = stripe.CheckoutSessionStatusExpired
	return &stripe.CheckoutSession{ID: id, Status: stripe.CheckoutSessionStatusExpired}, nil
}

// GetSubscription implements billing.API.
func (f *FakeAPI) GetSubscription(_ context.Context, id string) (*stripe.Subscription, error) {
	err := f.enter("GetSubscription")
	defer f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s, ok := f.Subs[id]
	if !ok {
		return nil, missing("subscription")
	}
	return s, nil
}

// ListSubscriptions lists every subscription but canceled ones, as Stripe does by default.
func (f *FakeAPI) ListSubscriptions(context.Context) ([]*stripe.Subscription, error) {
	err := f.enter("ListSubscriptions")
	defer f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var out []*stripe.Subscription
	for _, id := range slices.Sorted(maps.Keys(f.Subs)) {
		if s := f.Subs[id]; s.Status != stripe.SubscriptionStatusCanceled {
			out = append(out, s)
		}
	}
	return out, nil
}

// UpdateSubscription records the params and applies metadata and item changes like Stripe.
func (f *FakeAPI) UpdateSubscription(_ context.Context, id string, p *stripe.SubscriptionUpdateParams) (*stripe.Subscription, error) {
	err := f.enter("UpdateSubscription")
	defer f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s, ok := f.Subs[id]
	if !ok {
		return nil, missing("subscription")
	}
	f.Updates[id] = append(f.Updates[id], p)
	if p.Metadata != nil {
		s.Metadata = p.Metadata
	}
	for _, item := range p.Items {
		switch {
		case stripe.BoolValue(item.Deleted):
			s.Items.Data = slices.DeleteFunc(s.Items.Data, func(it *stripe.SubscriptionItem) bool { return it.ID == stripe.StringValue(item.ID) })
		case item.ID != nil:
			for _, it := range s.Items.Data {
				if it.ID == *item.ID && item.Quantity != nil {
					it.Quantity = *item.Quantity
				}
			}
		case item.Price != nil:
			s.Items.Data = append(s.Items.Data, &stripe.SubscriptionItem{ID: f.next("si"), Price: f.Prices[*item.Price], Quantity: stripe.Int64Value(item.Quantity)})
		}
	}
	return s, nil
}

// GetProduct implements billing.API.
func (f *FakeAPI) GetProduct(_ context.Context, id string) (*stripe.Product, error) {
	err := f.enter("GetProduct")
	defer f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	p, ok := f.Products[id]
	if !ok {
		return nil, missing("product")
	}
	return p, nil
}

// CreateProduct implements billing.API.
func (f *FakeAPI) CreateProduct(_ context.Context, p *stripe.ProductCreateParams) (*stripe.Product, error) {
	err := f.enter("CreateProduct")
	defer f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	prod := &stripe.Product{ID: stripe.StringValue(p.ID), Name: stripe.StringValue(p.Name), Active: true, Metadata: p.Metadata}
	if prod.ID == "" {
		prod.ID = f.next("prod")
	}
	f.Products[prod.ID] = prod
	return prod, nil
}

// UpdateProduct implements billing.API.
func (f *FakeAPI) UpdateProduct(_ context.Context, id string, p *stripe.ProductUpdateParams) (*stripe.Product, error) {
	err := f.enter("UpdateProduct")
	defer f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	prod, ok := f.Products[id]
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

// ListPrices returns every price, active and archived, in id order.
func (f *FakeAPI) ListPrices(context.Context) ([]*stripe.Price, error) {
	err := f.enter("ListPrices")
	defer f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var out []*stripe.Price
	for _, id := range slices.Sorted(maps.Keys(f.Prices)) {
		out = append(out, f.Prices[id])
	}
	return out, nil
}

// CreatePrice stores every field Seed compares and moves a lookup key only when asked.
func (f *FakeAPI) CreatePrice(_ context.Context, p *stripe.PriceCreateParams) (*stripe.Price, error) {
	err := f.enter("CreatePrice")
	defer f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	key := stripe.StringValue(p.LookupKey)
	for _, other := range f.Prices {
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
	f.Prices[price.ID] = price
	return price, nil
}

// UpdatePrice implements billing.API.
func (f *FakeAPI) UpdatePrice(_ context.Context, id string, p *stripe.PriceUpdateParams) (*stripe.Price, error) {
	err := f.enter("UpdatePrice")
	defer f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	price, ok := f.Prices[id]
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

// CreatePortalSession returns a fake portal URL naming the customer.
func (f *FakeAPI) CreatePortalSession(_ context.Context, p *stripe.BillingPortalSessionCreateParams) (*stripe.BillingPortalSession, error) {
	err := f.enter("CreatePortalSession")
	defer f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	f.Portals = append(f.Portals, p)
	return &stripe.BillingPortalSession{ID: f.next("bps"), URL: "https://billing.stripe.test/" + stripe.StringValue(p.Customer)}, nil
}

// PriceByKey finds the price carrying a lookup key, or nil.
func (f *FakeAPI) PriceByKey(key string) *stripe.Price {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.priceByKey(key)
}

func (f *FakeAPI) priceByKey(key string) *stripe.Price {
	for _, id := range slices.Sorted(maps.Keys(f.Prices)) {
		if p := f.Prices[id]; p.LookupKey == key {
			return p
		}
	}
	return nil
}

// Item is one subscription line: a price lookup key and a quantity.
type Item struct {
	Key string
	Qty int64
}

// AddSub installs a subscription whose items reference prices by lookup key.
func (f *FakeAPI) AddSub(t testing.TB, id, customerID string, status stripe.SubscriptionStatus, metadata map[string]string, periodEnd time.Time, items ...Item) *stripe.Subscription {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	s := &stripe.Subscription{ID: id, Status: status, Metadata: metadata, Customer: &stripe.Customer{ID: customerID}, Items: &stripe.SubscriptionItemList{}}
	for i, it := range items {
		p := f.priceByKey(it.Key)
		if p == nil {
			t.Fatalf("no price for %s; seed the fake first", it.Key)
		}
		s.Items.Data = append(s.Items.Data, &stripe.SubscriptionItem{
			ID: fmt.Sprintf("si_%s_%d", id, i), Price: p, Quantity: it.Qty,
			CurrentPeriodStart: periodStart(p, periodEnd).Unix(), CurrentPeriodEnd: periodEnd.Unix(),
		})
	}
	f.Subs[id] = s
	return s
}

// periodStart is periodEnd minus one billing interval of p, or periodEnd when p carries none.
func periodStart(p *stripe.Price, periodEnd time.Time) time.Time {
	if p.Recurring == nil {
		return periodEnd
	}
	n := int(p.Recurring.IntervalCount)
	switch p.Recurring.Interval {
	case stripe.PriceRecurringIntervalYear:
		return periodEnd.AddDate(-n, 0, 0)
	case stripe.PriceRecurringIntervalMonth:
		return periodEnd.AddDate(0, -n, 0)
	case stripe.PriceRecurringIntervalWeek:
		return periodEnd.AddDate(0, 0, -7*n)
	case stripe.PriceRecurringIntervalDay:
		return periodEnd.AddDate(0, 0, -n)
	}
	return periodEnd
}
