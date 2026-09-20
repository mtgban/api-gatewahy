package billing

import (
	"context"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
)

// Store is the slice of apiaccess.Client billing writes through.
type Store interface {
	GetAccount(ctx context.Context, id int64) (apiaccess.Account, error)
	GetAccountByStripeCustomer(ctx context.Context, customerID string) (apiaccess.Account, error)
	SetStripeCustomerID(ctx context.Context, accountID int64, customerID string) (string, error)
	ConsumeInvite(ctx context.Context, token, email string, now time.Time) (apiaccess.Invite, error)
	ReleaseInvite(ctx context.Context, token string) error
	UpsertStripeEntitlement(ctx context.Context, e apiaccess.Entitlement) (apiaccess.Entitlement, error)
	ListActiveStripeRefs(ctx context.Context) ([]string, error)
	Notify(ctx context.Context, payload string) error
}

var _ Store = (*apiaccess.Client)(nil)
