package billing

import (
	"context"
	"errors"
	"fmt"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/stripe/stripe-go/v84"
)

// ErrNoCustomer means the account never went through checkout.
var ErrNoCustomer = errors.New("billing: account has no Stripe customer")

// PortalURL creates a Customer Portal session and returns its URL.
func PortalURL(ctx context.Context, api API, account apiaccess.Account, returnURL string) (string, error) {
	if account.StripeCustomerID == "" {
		return "", ErrNoCustomer
	}
	sess, err := api.CreatePortalSession(ctx, &stripe.BillingPortalSessionCreateParams{
		Customer:  stripe.String(account.StripeCustomerID),
		ReturnURL: stripe.String(returnURL),
	})
	if err != nil {
		return "", fmt.Errorf("billing: portal session: %w", err)
	}
	return sess.URL, nil
}
