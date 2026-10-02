package billingtest

import (
	"context"
	"testing"

	"github.com/stripe/stripe-go/v84"
)

func TestFakeTransfersLookupKey(t *testing.T) {
	f := NewFakeAPI()
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
	if old.LookupKey != "" || replacement.LookupKey != "k" || f.PriceByKey("k").ID != replacement.ID {
		t.Errorf("transfer: old %q new %q", old.LookupKey, replacement.LookupKey)
	}
}
