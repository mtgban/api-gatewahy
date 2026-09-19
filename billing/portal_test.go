package billing

import (
	"context"
	"errors"
	"testing"

	"github.com/stripe/stripe-go/v84"
)

func TestPortalURL(t *testing.T) {
	f := newFakeAPI()
	ctx := context.Background()
	if _, err := PortalURL(ctx, f, testAccount, "https://api.mtgban.com/"); !errors.Is(err, ErrNoCustomer) {
		t.Errorf("no customer: %v", err)
	}
	withCustomer := testAccount
	withCustomer.StripeCustomerID = "cus_7"
	url, err := PortalURL(ctx, f, withCustomer, "https://api.mtgban.com/")
	if err != nil || url != "https://billing.stripe.test/cus_7" {
		t.Errorf("url %q %v", url, err)
	}
	if len(f.portals) != 1 || stripe.StringValue(f.portals[0].Customer) != "cus_7" || stripe.StringValue(f.portals[0].ReturnURL) != "https://api.mtgban.com/" {
		t.Errorf("params %+v", f.portals)
	}
	f.fail["CreatePortalSession"] = errors.New("stripe down")
	if _, err := PortalURL(ctx, f, withCustomer, "https://api.mtgban.com/"); err == nil {
		t.Error("stripe failure swallowed")
	}
}
