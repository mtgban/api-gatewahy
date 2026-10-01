package billing

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/mtgban-website/apiproductlist"
	"github.com/stripe/stripe-go/v84"
)

// ErrChangeNotReconciled means Stripe took the plan change but the entitlement
// row did not follow; the webhook or the nightly pass catches it up.
var ErrChangeNotReconciled = errors.New("billing: plan changed, access not updated yet")

// ChangePlan prorates the subscription onto newPlan at its own interval, then reconciles.
// The resolved plan returns even with an error; a failed reconcile is ErrChangeNotReconciled.
func ChangePlan(ctx context.Context, api API, cat *apiproductlist.ProductList, stores StoreLister, games []string, account apiaccess.Account,
	subID string, newPlan Plan, reconcile func(context.Context, string) error) (ResolvedPlan, error) {
	sub, err := api.GetSubscription(ctx, subID)
	if err != nil {
		return ResolvedPlan{}, fmt.Errorf("billing: fetch %s: %w", subID, err)
	}
	current, accountID, err := PlanFromMetadata(sub.Metadata)
	if err != nil {
		return ResolvedPlan{}, err
	}
	ownsByCustomer := sub.Customer != nil && account.StripeCustomerID != "" && sub.Customer.ID == account.StripeCustomerID
	if accountID != account.ID && !ownsByCustomer {
		return ResolvedPlan{}, fmt.Errorf("billing: subscription %s does not belong to account %d", subID, account.ID)
	}
	newPlan.Interval = current.Interval
	// The interval was already authorized when the subscription began.
	newPlan, err = newPlan.Validate(cat, games, true)
	if err != nil {
		return ResolvedPlan{}, err
	}
	resolved, err := newPlan.Resolve(ctx, cat, stores)
	if err != nil {
		return ResolvedPlan{}, err
	}
	ids, err := priceIDs(ctx, api)
	if err != nil {
		return resolved, fmt.Errorf("billing: list prices: %w", err)
	}
	want := planItems(cat, newPlan)
	var items []*stripe.SubscriptionUpdateItemParams
	if sub.Items != nil {
		for _, it := range sub.Items.Data {
			key := ""
			if it.Price != nil {
				// A replaced price keeps its key in metadata, so the item stays at its price.
				key = priceLookupKey(it.Price)
			}
			qty, keep := want[key]
			switch {
			case !keep:
				items = append(items, &stripe.SubscriptionUpdateItemParams{ID: stripe.String(it.ID), Deleted: stripe.Bool(true)})
			case qty != it.Quantity:
				items = append(items, &stripe.SubscriptionUpdateItemParams{ID: stripe.String(it.ID), Quantity: stripe.Int64(qty)})
			}
			delete(want, key)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(want)) {
		id, ok := ids[key]
		if !ok {
			return resolved, fmt.Errorf("%w: %s", ErrPriceNotSeeded, key)
		}
		items = append(items, &stripe.SubscriptionUpdateItemParams{Price: stripe.String(id), Quantity: stripe.Int64(want[key])})
	}
	proration := prorationBehavior(current, newPlan, cat)
	update := &stripe.SubscriptionUpdateParams{
		Items:             items,
		Metadata:          newPlan.Metadata(account.ID),
		ProrationBehavior: stripe.String(proration),
	}
	if proration == "always_invoice" {
		// A declined upgrade charge must fail here, not move the subscription to past_due.
		update.PaymentBehavior = stripe.String("error_if_incomplete")
	}
	if _, err := api.UpdateSubscription(ctx, subID, update); err != nil {
		return resolved, fmt.Errorf("billing: update %s: %w", subID, err)
	}
	if err := reconcile(ctx, subID); err != nil {
		return resolved, fmt.Errorf("%w: %w", ErrChangeNotReconciled, err)
	}
	return resolved, nil
}

// prorationBehavior invoices an upgrade immediately, so a cancel right after
// cannot dodge it; a downgrade still credits the next invoice.
func prorationBehavior(current, next Plan, cat *apiproductlist.ProductList) string {
	currentTotal, err := current.Total(cat)
	if err != nil {
		return "create_prorations" // no total, proceed as a non-upgrade
	}
	nextTotal, err := next.Total(cat)
	if err != nil {
		return "create_prorations" // no total, proceed as a non-upgrade
	}
	if nextTotal > currentTotal {
		return "always_invoice"
	}
	return "create_prorations"
}

// IsPaymentFailure reports whether Stripe refused a change because the
// charge failed (a 402, as error_if_incomplete returns on a declined card).
func IsPaymentFailure(err error) bool {
	var se *stripe.Error
	return errors.As(err, &se) && se.HTTPStatusCode == 402
}
