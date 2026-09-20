package billing

import (
	"context"
	"errors"
	"testing"

	"github.com/stripe/stripe-go/v84"
)

func TestIsMissing(t *testing.T) {
	if !IsMissing(missing("thing")) {
		t.Error("resource_missing not recognized")
	}
	if IsMissing(&stripe.Error{Code: stripe.ErrorCodeIdempotencyKeyInUse}) || IsMissing(errors.New("x")) || IsMissing(nil) {
		t.Error("other errors reported as missing")
	}
}

func TestFakeTransfersLookupKey(t *testing.T) {
	f := newFakeAPI()
	ctx := context.Background()
	old, err := f.CreatePrice(ctx, &stripe.PriceCreateParams{LookupKey: stripe.String("k"), UnitAmount: stripe.Int64(100)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.CreatePrice(ctx, &stripe.PriceCreateParams{LookupKey: stripe.String("k"), UnitAmount: stripe.Int64(200)}); err == nil {
		t.Error("duplicate lookup key accepted without transfer")
	}
	replacement, err := f.CreatePrice(ctx, &stripe.PriceCreateParams{LookupKey: stripe.String("k"), UnitAmount: stripe.Int64(200), TransferLookupKey: stripe.Bool(true)})
	if err != nil {
		t.Fatal(err)
	}
	if old.LookupKey != "" || replacement.LookupKey != "k" || f.priceByKey("k").ID != replacement.ID {
		t.Errorf("transfer: old %q new %q", old.LookupKey, replacement.LookupKey)
	}
}

func TestPriceIDsSkipsArchived(t *testing.T) {
	f := newFakeAPI()
	ctx := context.Background()
	live, _ := f.CreatePrice(ctx, &stripe.PriceCreateParams{LookupKey: stripe.String("live"), UnitAmount: stripe.Int64(100)})
	dead, _ := f.CreatePrice(ctx, &stripe.PriceCreateParams{LookupKey: stripe.String("dead"), UnitAmount: stripe.Int64(100)})
	if _, err := f.UpdatePrice(ctx, dead.ID, &stripe.PriceUpdateParams{Active: stripe.Bool(false)}); err != nil {
		t.Fatal(err)
	}
	ids, err := priceIDs(ctx, f)
	if err != nil || len(ids) != 1 || ids["live"] != live.ID {
		t.Errorf("ids %v %v", ids, err)
	}
}

// seq2 builds a stripe.Seq2 that yields one price then stops.
func seq2(p *stripe.Price, err error) stripe.Seq2[*stripe.Price, error] {
	return func(yield func(*stripe.Price, error) bool) {
		yield(p, err)
	}
}

func TestListPricesQueriesActiveThenArchived(t *testing.T) {
	var gotActive []bool
	list := func(p *stripe.PriceListParams) stripe.Seq2[*stripe.Price, error] {
		gotActive = append(gotActive, stripe.BoolValue(p.Active))
		id := "archived"
		if stripe.BoolValue(p.Active) {
			id = "active"
		}
		return seq2(&stripe.Price{ID: id}, nil)
	}
	prices, err := listPrices(context.Background(), list)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotActive) != 2 || !gotActive[0] || gotActive[1] {
		t.Errorf("active values %v, want [true false]", gotActive)
	}
	if len(prices) != 2 || prices[0].ID != "active" || prices[1].ID != "archived" {
		t.Errorf("prices %v", prices)
	}
}

func TestListPricesReturnsSecondPassError(t *testing.T) {
	wantErr := errors.New("boom")
	list := func(p *stripe.PriceListParams) stripe.Seq2[*stripe.Price, error] {
		if stripe.BoolValue(p.Active) {
			return seq2(&stripe.Price{ID: "active"}, nil)
		}
		return seq2(nil, wantErr)
	}
	if _, err := listPrices(context.Background(), list); !errors.Is(err, wantErr) {
		t.Errorf("err %v, want %v", err, wantErr)
	}
}
