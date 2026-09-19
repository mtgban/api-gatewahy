package billing

import (
	"context"
	"sort"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
)

// memStore is Store in memory. Invites are keyed by the plaintext token.
type memStore struct {
	accounts   map[int64]apiaccess.Account
	invites    map[string]*apiaccess.Invite
	ents       map[string]apiaccess.Entitlement
	nextEnt    int64
	notified   int
	failUpsert error
}

func newMemStore(accounts ...apiaccess.Account) *memStore {
	s := &memStore{accounts: map[int64]apiaccess.Account{}, invites: map[string]*apiaccess.Invite{}, ents: map[string]apiaccess.Entitlement{}}
	for _, a := range accounts {
		s.accounts[a.ID] = a
	}
	return s
}

func (s *memStore) addInvite(token, intervalKey, email string, expires time.Time) {
	s.invites[token] = &apiaccess.Invite{TokenHash: token, IntervalKey: intervalKey, Email: apiaccess.NormalizeEmail(email), ExpiresAt: expires}
}

func (s *memStore) GetAccount(_ context.Context, id int64) (apiaccess.Account, error) {
	a, ok := s.accounts[id]
	if !ok {
		return apiaccess.Account{}, apiaccess.ErrNotFound
	}
	return a, nil
}

func (s *memStore) GetAccountByStripeCustomer(_ context.Context, customerID string) (apiaccess.Account, error) {
	for _, a := range s.accounts {
		if customerID != "" && a.StripeCustomerID == customerID {
			return a, nil
		}
	}
	return apiaccess.Account{}, apiaccess.ErrNotFound
}

func (s *memStore) SetStripeCustomerID(_ context.Context, id int64, customerID string) (string, error) {
	a, ok := s.accounts[id]
	if !ok {
		return "", apiaccess.ErrNotFound
	}
	if a.StripeCustomerID == "" {
		a.StripeCustomerID = customerID
		s.accounts[id] = a
	}
	return a.StripeCustomerID, nil
}

func (s *memStore) ConsumeInvite(_ context.Context, token, email string, now time.Time) (apiaccess.Invite, error) {
	inv, ok := s.invites[token]
	if !ok || inv.UsedAt != nil || !inv.ExpiresAt.After(now) || (inv.Email != "" && inv.Email != apiaccess.NormalizeEmail(email)) {
		return apiaccess.Invite{}, apiaccess.ErrInviteInvalid
	}
	inv.UsedAt = &now
	return *inv, nil
}

func (s *memStore) ReleaseInvite(_ context.Context, token string) error {
	if inv, ok := s.invites[token]; ok {
		inv.UsedAt = nil
	}
	return nil
}

func (s *memStore) UpsertStripeEntitlement(_ context.Context, e apiaccess.Entitlement) (apiaccess.Entitlement, error) {
	if s.failUpsert != nil {
		return apiaccess.Entitlement{}, s.failUpsert
	}
	if old, ok := s.ents[e.ExternalRef]; ok {
		e.ID, e.ValidFrom = old.ID, old.ValidFrom
	} else {
		s.nextEnt++
		e.ID, e.ValidFrom = s.nextEnt, time.Now()
	}
	s.ents[e.ExternalRef] = e
	return e, nil
}

func (s *memStore) ListActiveStripeRefs(context.Context) ([]string, error) {
	var out []string
	for ref, e := range s.ents {
		if e.Status == "active" {
			out = append(out, ref)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (s *memStore) Notify(context.Context, string) error {
	s.notified++
	return nil
}
