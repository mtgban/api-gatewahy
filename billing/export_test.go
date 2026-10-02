package billing

import (
	"context"
	"time"

	"github.com/mtgban/mtgban-website/apiproductlist"
	"github.com/stripe/stripe-go/v84"
)

// Internals the external billing_test package exercises.
var (
	PriceLookupKey = priceLookupKey
	PriceIDs       = priceIDs
)

// SetReleaseTimeout bounds invite releases at d; the returned func restores the bound.
func SetReleaseTimeout(d time.Duration) (restore func()) {
	saved := releaseTimeout
	releaseTimeout = d
	return func() { releaseTimeout = saved }
}

// ListPricesFrom runs listPrices over list.
func ListPricesFrom(ctx context.Context, list func(*stripe.PriceListParams) stripe.Seq2[*stripe.Price, error]) ([]*stripe.Price, error) {
	return listPrices(ctx, list)
}

// SpecLookupKeys lists the lookup key of every price Seed would create for cat.
func SpecLookupKeys(cat *apiproductlist.ProductList) ([]string, error) {
	specs, err := priceSpecs(cat)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(specs))
	for _, s := range specs {
		keys = append(keys, s.lookupKey)
	}
	return keys, nil
}
