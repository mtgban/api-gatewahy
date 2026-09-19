package billing

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/mtgban-website/apiproductlist"
	"github.com/stripe/stripe-go/v84"
)

// ChangePlan rewrites the subscription's items and metadata to newPlan with
// proration, then runs reconcile. The interval stays what the subscription has.
func ChangePlan(ctx context.Context, api API, cat *apiproductlist.ProductList, games []string, account apiaccess.Account,
	subID string, newPlan Plan, reconcile func(context.Context, string) error) (Plan, error) {
	sub, err := api.GetSubscription(ctx, subID)
	if err != nil {
		return Plan{}, fmt.Errorf("billing: fetch %s: %w", subID, err)
	}
	current, accountID, err := PlanFromMetadata(sub.Metadata)
	if err != nil {
		return Plan{}, err
	}
	ownsByCustomer := sub.Customer != nil && account.StripeCustomerID != "" && sub.Customer.ID == account.StripeCustomerID
	if accountID != account.ID && !ownsByCustomer {
		return Plan{}, fmt.Errorf("billing: subscription %s does not belong to account %d", subID, account.ID)
	}
	newPlan.Interval = current.Interval
	// The interval was already authorized when the subscription began.
	newPlan, err = newPlan.Validate(cat, games, true)
	if err != nil {
		return Plan{}, err
	}
	ids, err := priceIDs(ctx, api)
	if err != nil {
		return Plan{}, fmt.Errorf("billing: list prices: %w", err)
	}
	want := map[string]int64{}
	for _, li := range newPlan.LineItems(cat) {
		want[li.LookupKey] = li.Quantity
	}
	var items []*stripe.SubscriptionUpdateItemParams
	if sub.Items != nil {
		for _, it := range sub.Items.Data {
			key := ""
			if it.Price != nil {
				key = it.Price.LookupKey
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
			return Plan{}, fmt.Errorf("%w: %s", ErrPriceNotSeeded, key)
		}
		items = append(items, &stripe.SubscriptionUpdateItemParams{Price: stripe.String(id), Quantity: stripe.Int64(want[key])})
	}
	if _, err := api.UpdateSubscription(ctx, subID, &stripe.SubscriptionUpdateParams{
		Items:             items,
		Metadata:          newPlan.Metadata(account.ID),
		ProrationBehavior: stripe.String("create_prorations"),
	}); err != nil {
		return Plan{}, fmt.Errorf("billing: update %s: %w", subID, err)
	}
	return newPlan, reconcile(ctx, subID)
}
