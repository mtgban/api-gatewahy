package billing

import (
	"context"
	"errors"

	"github.com/stripe/stripe-go/v84"
)

// API is the slice of Stripe the gateway calls. Tests implement it in memory.
type API interface {
	CreateCustomer(ctx context.Context, params *stripe.CustomerCreateParams) (*stripe.Customer, error)
	CreateCheckoutSession(ctx context.Context, params *stripe.CheckoutSessionCreateParams) (*stripe.CheckoutSession, error)
	GetSubscription(ctx context.Context, id string) (*stripe.Subscription, error)
	ListSubscriptions(ctx context.Context) ([]*stripe.Subscription, error)
	UpdateSubscription(ctx context.Context, id string, params *stripe.SubscriptionUpdateParams) (*stripe.Subscription, error)
	GetProduct(ctx context.Context, id string) (*stripe.Product, error)
	CreateProduct(ctx context.Context, params *stripe.ProductCreateParams) (*stripe.Product, error)
	UpdateProduct(ctx context.Context, id string, params *stripe.ProductUpdateParams) (*stripe.Product, error)
	ListPrices(ctx context.Context) ([]*stripe.Price, error)
	CreatePrice(ctx context.Context, params *stripe.PriceCreateParams) (*stripe.Price, error)
	UpdatePrice(ctx context.Context, id string, params *stripe.PriceUpdateParams) (*stripe.Price, error)
	CreatePortalSession(ctx context.Context, params *stripe.BillingPortalSessionCreateParams) (*stripe.BillingPortalSession, error)
}

// Client is API over the real stripe-go client.
type Client struct {
	sc *stripe.Client
}

var _ API = (*Client)(nil)

// NewClient returns a Client for the secret key.
func NewClient(secretKey string) *Client {
	return &Client{sc: stripe.NewClient(secretKey)}
}

// listLimit is Stripe's maximum page size; the iterator pages past it.
const listLimit = 100

// CreateCustomer implements API.
func (c *Client) CreateCustomer(ctx context.Context, params *stripe.CustomerCreateParams) (*stripe.Customer, error) {
	return c.sc.V1Customers.Create(ctx, params)
}

// CreateCheckoutSession implements API.
func (c *Client) CreateCheckoutSession(ctx context.Context, params *stripe.CheckoutSessionCreateParams) (*stripe.CheckoutSession, error) {
	return c.sc.V1CheckoutSessions.Create(ctx, params)
}

// GetSubscription fetches one subscription with its item prices expanded.
func (c *Client) GetSubscription(ctx context.Context, id string) (*stripe.Subscription, error) {
	return c.sc.V1Subscriptions.Retrieve(ctx, id, &stripe.SubscriptionRetrieveParams{
		Expand: []*string{stripe.String("items.data.price")},
	})
}

// ListSubscriptions returns every subscription Stripe lists by default,
// which excludes canceled ones, with item prices expanded.
func (c *Client) ListSubscriptions(ctx context.Context) ([]*stripe.Subscription, error) {
	params := &stripe.SubscriptionListParams{}
	params.Limit = stripe.Int64(listLimit)
	params.Expand = []*string{stripe.String("data.items.data.price")}
	var out []*stripe.Subscription
	for s, err := range c.sc.V1Subscriptions.List(ctx, params) {
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// UpdateSubscription implements API.
func (c *Client) UpdateSubscription(ctx context.Context, id string, params *stripe.SubscriptionUpdateParams) (*stripe.Subscription, error) {
	return c.sc.V1Subscriptions.Update(ctx, id, params)
}

// GetProduct implements API.
func (c *Client) GetProduct(ctx context.Context, id string) (*stripe.Product, error) {
	return c.sc.V1Products.Retrieve(ctx, id, nil)
}

// CreateProduct implements API.
func (c *Client) CreateProduct(ctx context.Context, params *stripe.ProductCreateParams) (*stripe.Product, error) {
	return c.sc.V1Products.Create(ctx, params)
}

// UpdateProduct implements API.
func (c *Client) UpdateProduct(ctx context.Context, id string, params *stripe.ProductUpdateParams) (*stripe.Product, error) {
	return c.sc.V1Products.Update(ctx, id, params)
}

// ListPrices returns active and archived prices; Stripe lists only active ones unless asked.
func (c *Client) ListPrices(ctx context.Context) ([]*stripe.Price, error) {
	return listPrices(ctx, func(p *stripe.PriceListParams) stripe.Seq2[*stripe.Price, error] {
		return c.sc.V1Prices.List(ctx, p)
	})
}

// listPrices runs list once for active prices and once for archived ones.
func listPrices(ctx context.Context, list func(*stripe.PriceListParams) stripe.Seq2[*stripe.Price, error]) ([]*stripe.Price, error) {
	var out []*stripe.Price
	for _, active := range []bool{true, false} {
		params := &stripe.PriceListParams{Active: stripe.Bool(active)}
		params.Limit = stripe.Int64(listLimit)
		for p, err := range list(params) {
			if err != nil {
				return nil, err
			}
			out = append(out, p)
		}
	}
	return out, nil
}

// CreatePrice implements API.
func (c *Client) CreatePrice(ctx context.Context, params *stripe.PriceCreateParams) (*stripe.Price, error) {
	return c.sc.V1Prices.Create(ctx, params)
}

// UpdatePrice implements API.
func (c *Client) UpdatePrice(ctx context.Context, id string, params *stripe.PriceUpdateParams) (*stripe.Price, error) {
	return c.sc.V1Prices.Update(ctx, id, params)
}

// CreatePortalSession implements API.
func (c *Client) CreatePortalSession(ctx context.Context, params *stripe.BillingPortalSessionCreateParams) (*stripe.BillingPortalSession, error) {
	return c.sc.V1BillingPortalSessions.Create(ctx, params)
}

// IsMissing reports whether err is Stripe saying the object does not exist.
func IsMissing(err error) bool {
	var se *stripe.Error
	return errors.As(err, &se) && se.Code == stripe.ErrorCodeResourceMissing
}

// priceIDs maps every active lookup key to its price id.
func priceIDs(ctx context.Context, api API) (map[string]string, error) {
	prices, err := api.ListPrices(ctx)
	if err != nil {
		return nil, err
	}
	ids := map[string]string{}
	for _, p := range prices {
		if p.Active && p.LookupKey != "" {
			ids[p.LookupKey] = p.ID
		}
	}
	return ids, nil
}
