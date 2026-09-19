package billing

import (
	"context"
	"fmt"
	"strings"

	"github.com/mtgban/mtgban-website/apiproductlist"
	"github.com/stripe/stripe-go/v84"
)

// ProductID is the Stripe product id for a catalog item; fixed so seed is idempotent.
func ProductID(key string) string {
	return "mtgban_api_" + key
}

// SeedResult lists what Seed changed, by product id or lookup key.
type SeedResult struct {
	Created  []string
	Updated  []string
	Archived []string
}

type productSpec struct {
	id, name, kind string
}

type priceSpec struct {
	lookupKey string
	productID string
	amount    int64
	interval  apiproductlist.Interval
	metadata  map[string]string
}

func productSpecs(cat *apiproductlist.ProductList) []productSpec {
	var out []productSpec
	for _, p := range cat.Packages {
		out = append(out, productSpec{ProductID(p.Key), p.Name, "package"})
	}
	for _, a := range cat.Addons {
		out = append(out, productSpec{ProductID(a.Key), a.Name, "addon"})
	}
	return out
}

func priceSpecs(cat *apiproductlist.ProductList) ([]priceSpec, error) {
	var out []priceSpec
	for _, p := range cat.Packages {
		for _, iv := range cat.Intervals {
			amount, err := iv.Amount(p.Monthly)
			if err != nil {
				return nil, err
			}
			out = append(out, priceSpec{
				lookupKey: apiproductlist.LookupKey(p.Key, iv.Key), productID: ProductID(p.Key), amount: amount, interval: iv,
				metadata: map[string]string{"kind": "package", "package": p.Key, "store_scope": p.StoreScope, "modes": strings.Join(p.Modes, ","), "interval_key": iv.Key},
			})
		}
	}
	for _, a := range cat.Addons {
		for _, iv := range cat.Intervals {
			amount, err := iv.Amount(a.Monthly)
			if err != nil {
				return nil, err
			}
			out = append(out, priceSpec{
				lookupKey: apiproductlist.LookupKey(a.Key, iv.Key), productID: ProductID(a.Key), amount: amount, interval: iv,
				metadata: map[string]string{"kind": "addon", "addon": a.Key, "applies_to": strings.Join(a.AppliesTo, ","), "interval_key": iv.Key},
			})
		}
	}
	return out, nil
}

// matches reports whether p already is this spec, amount and shape included.
func (s priceSpec) matches(p *stripe.Price, currency string) bool {
	return p.UnitAmount == s.amount && string(p.Currency) == currency && p.Recurring != nil &&
		string(p.Recurring.Interval) == s.interval.Interval && p.Recurring.IntervalCount == s.interval.Count &&
		p.Product != nil && p.Product.ID == s.productID
}

// metadataMatches reports whether have carries every key in want with the
// same value. Real Stripe merges metadata on update rather than replacing
// it, so a stray hand-set key in have must not count as drift.
func metadataMatches(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

func (s priceSpec) createParams(currency string, transfer bool) *stripe.PriceCreateParams {
	params := &stripe.PriceCreateParams{
		Currency:   stripe.String(currency),
		UnitAmount: stripe.Int64(s.amount),
		Product:    stripe.String(s.productID),
		LookupKey:  stripe.String(s.lookupKey),
		Nickname:   stripe.String(s.lookupKey),
		Metadata:   s.metadata,
		Recurring: &stripe.PriceCreateRecurringParams{
			Interval:      stripe.String(s.interval.Interval),
			IntervalCount: stripe.Int64(s.interval.Count),
		},
	}
	if transfer {
		params.TransferLookupKey = stripe.Bool(true)
	}
	return params
}

// Seed creates or updates one Product per catalog item and one Price per
// item and interval. A changed amount creates a replacement Price that
// takes over the lookup key and archives the old one.
func Seed(ctx context.Context, api API, cat *apiproductlist.ProductList) (SeedResult, error) {
	var res SeedResult
	for _, spec := range productSpecs(cat) {
		existing, err := api.GetProduct(ctx, spec.id)
		switch {
		case IsMissing(err):
			_, err = api.CreateProduct(ctx, &stripe.ProductCreateParams{
				ID: stripe.String(spec.id), Name: stripe.String(spec.name),
				Metadata: map[string]string{"kind": spec.kind, "key": strings.TrimPrefix(spec.id, "mtgban_api_")},
			})
			if err != nil {
				return res, fmt.Errorf("create product %s: %w", spec.id, err)
			}
			res.Created = append(res.Created, spec.id)
		case err != nil:
			return res, fmt.Errorf("get product %s: %w", spec.id, err)
		case existing.Name != spec.name || !existing.Active:
			_, err = api.UpdateProduct(ctx, spec.id, &stripe.ProductUpdateParams{Name: stripe.String(spec.name), Active: stripe.Bool(true)})
			if err != nil {
				return res, fmt.Errorf("update product %s: %w", spec.id, err)
			}
			res.Updated = append(res.Updated, spec.id)
		}
	}

	prices, err := api.ListPrices(ctx)
	if err != nil {
		return res, fmt.Errorf("list prices: %w", err)
	}
	byKey := map[string]*stripe.Price{}
	for _, p := range prices {
		if p.LookupKey != "" {
			byKey[p.LookupKey] = p
		}
	}
	specs, err := priceSpecs(cat)
	if err != nil {
		return res, fmt.Errorf("price specs: %w", err)
	}
	for _, spec := range specs {
		existing := byKey[spec.lookupKey]
		switch {
		case existing == nil:
			if _, err := api.CreatePrice(ctx, spec.createParams(cat.Currency, false)); err != nil {
				return res, fmt.Errorf("create price %s: %w", spec.lookupKey, err)
			}
			res.Created = append(res.Created, spec.lookupKey)
		case spec.matches(existing, cat.Currency):
			if existing.Active && metadataMatches(existing.Metadata, spec.metadata) {
				continue
			}
			if _, err := api.UpdatePrice(ctx, existing.ID, &stripe.PriceUpdateParams{Active: stripe.Bool(true), Metadata: spec.metadata}); err != nil {
				return res, fmt.Errorf("update price %s: %w", spec.lookupKey, err)
			}
			res.Updated = append(res.Updated, spec.lookupKey)
		default:
			if _, err := api.CreatePrice(ctx, spec.createParams(cat.Currency, true)); err != nil {
				return res, fmt.Errorf("replace price %s: %w", spec.lookupKey, err)
			}
			if _, err := api.UpdatePrice(ctx, existing.ID, &stripe.PriceUpdateParams{Active: stripe.Bool(false)}); err != nil {
				return res, fmt.Errorf("archive price %s: %w", existing.ID, err)
			}
			res.Created = append(res.Created, spec.lookupKey)
			res.Archived = append(res.Archived, existing.ID)
		}
	}
	return res, nil
}
