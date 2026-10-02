package billing_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mtgban/api-gatewahy/billing"
	"github.com/mtgban/api-gatewahy/billing/billingtest"
	"github.com/stripe/stripe-go/v84"
)

func TestIsMissing(t *testing.T) {
	if !billing.IsMissing(&stripe.Error{Code: stripe.ErrorCodeResourceMissing, Msg: "no such thing", HTTPStatusCode: 404}) {
		t.Error("resource_missing not recognized")
	}
	if billing.IsMissing(&stripe.Error{Code: stripe.ErrorCodeIdempotencyKeyInUse}) || billing.IsMissing(errors.New("x")) || billing.IsMissing(nil) {
		t.Error("other errors reported as missing")
	}
}

func TestPriceIDsSkipsArchived(t *testing.T) {
	f := billingtest.NewFakeAPI()
	ctx := context.Background()
	live, _ := f.CreatePrice(ctx, &stripe.PriceCreateParams{LookupKey: stripe.String("live"), UnitAmount: stripe.Int64(100)})
	dead, _ := f.CreatePrice(ctx, &stripe.PriceCreateParams{LookupKey: stripe.String("dead"), UnitAmount: stripe.Int64(100)})
	if _, err := f.UpdatePrice(ctx, dead.ID, &stripe.PriceUpdateParams{Active: stripe.Bool(false)}); err != nil {
		t.Fatal(err)
	}
	ids, err := billing.PriceIDs(ctx, f)
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
	prices, err := billing.ListPricesFrom(context.Background(), list)
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
	if _, err := billing.ListPricesFrom(context.Background(), list); !errors.Is(err, wantErr) {
		t.Errorf("err %v, want %v", err, wantErr)
	}
}
