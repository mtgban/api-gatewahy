package billing

import (
	"errors"
	"fmt"
	"strings"

	"github.com/mtgban/api-gatewahy/apiaccess"
)

// SubscriptionFor picks the account's one active Stripe subscription from its entitlements.
func SubscriptionFor(ents []apiaccess.Entitlement) (string, error) {
	var refs []string
	for _, e := range ents {
		if e.Source == "stripe" && e.Status == "active" && e.ExternalRef != "" {
			refs = append(refs, e.ExternalRef)
		}
	}
	switch len(refs) {
	case 0:
		return "", errors.New("account has no active Stripe subscription")
	case 1:
		return refs[0], nil
	}
	return "", fmt.Errorf("account has %d active Stripe subscriptions (%s); name one with -sub", len(refs), strings.Join(refs, ", "))
}
