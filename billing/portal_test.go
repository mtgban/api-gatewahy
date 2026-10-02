package billing_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mtgban/api-gatewahy/billing"
	"github.com/mtgban/api-gatewahy/billing/billingtest"
	"github.com/stripe/stripe-go/v84"
)

func TestPortalURL(t *testing.T) {
	f := billingtest.NewFakeAPI()
	ctx := context.Background()
	if _, err := billing.PortalURL(ctx, f, testAccount, "https://api.mtgban.com/"); !errors.Is(err, billing.ErrNoCustomer) {
		t.Errorf("no customer: %v", err)
	}
	withCustomer := testAccount
	withCustomer.StripeCustomerID = "cus_7"
	url, err := billing.PortalURL(ctx, f, withCustomer, "https://api.mtgban.com/")
	if err != nil || url != "https://billing.stripe.test/cus_7" {
		t.Errorf("url %q %v", url, err)
	}
	if len(f.Portals) != 1 || stripe.StringValue(f.Portals[0].Customer) != "cus_7" || stripe.StringValue(f.Portals[0].ReturnURL) != "https://api.mtgban.com/" {
		t.Errorf("params %+v", f.Portals)
	}
	f.Fail["CreatePortalSession"] = errors.New("stripe down")
	if _, err := billing.PortalURL(ctx, f, withCustomer, "https://api.mtgban.com/"); err == nil {
		t.Error("stripe failure swallowed")
	}
}
