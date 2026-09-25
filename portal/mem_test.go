package portal

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/api-gatewahy/billing"
)

type memLink struct {
	accountID int64
	expires   time.Time
	used      bool
}

// memUsage pairs a usage row with the timestamp SummarizeUsage filters on.
type memUsage struct {
	Ts  time.Time
	Row apiaccess.UsageRow
}

var _ billing.Store = (*memStore)(nil)

type memStore struct {
	mu       sync.Mutex
	nextID   int64
	accounts map[int64]apiaccess.Account
	links    map[string]*memLink
	keys     map[int64]*apiaccess.Key
	ents     map[int64]*apiaccess.Entitlement
	trials   map[int64]*apiaccess.Trial
	invites  map[string]apiaccess.Invite
	usage    []memUsage
	notified []string
	nonces   map[string]time.Time
	actions  []apiaccess.AdminAction
	// clock is the test server's frozen clock; nil means real time.
	clock func() time.Time

	// entitlementErr, when set, is what AddEntitlement returns instead of succeeding.
	entitlementErr error
	// listKeysErr, listEntitlementsErr, listActionsErr, and usageErr inject failures for their namesakes.
	listKeysErr         error
	listEntitlementsErr error
	listActionsErr      error
	usageErr            error
}

func newMemStore() *memStore {
	return &memStore{accounts: map[int64]apiaccess.Account{}, links: map[string]*memLink{}, keys: map[int64]*apiaccess.Key{},
		ents: map[int64]*apiaccess.Entitlement{}, trials: map[int64]*apiaccess.Trial{}, invites: map[string]apiaccess.Invite{}, nonces: map[string]time.Time{}}
}

func (m *memStore) id() int64 { m.nextID++; return m.nextID }

// nowOr returns the frozen clock when the test server set one.
func (m *memStore) nowOr() time.Time {
	if m.clock != nil {
		return m.clock()
	}
	return time.Now()
}

func (m *memStore) GetAccount(_ context.Context, id int64) (apiaccess.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.accounts[id]
	if !ok {
		return apiaccess.Account{}, apiaccess.ErrNotFound
	}
	return a, nil
}

func (m *memStore) BumpSessionEpoch(_ context.Context, id int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.accounts[id]
	if !ok {
		return 0, apiaccess.ErrNotFound
	}
	a.SessionEpoch++
	m.accounts[id] = a
	return a.SessionEpoch, nil
}

func (m *memStore) GetAccountByEmail(_ context.Context, email string) (apiaccess.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.accounts {
		if a.Email == apiaccess.NormalizeEmail(email) {
			return a, nil
		}
	}
	return apiaccess.Account{}, apiaccess.ErrNotFound
}

func (m *memStore) GetAccountByStripeCustomer(_ context.Context, customerID string) (apiaccess.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.accounts {
		if a.StripeCustomerID == customerID {
			return a, nil
		}
	}
	return apiaccess.Account{}, apiaccess.ErrNotFound
}

func (m *memStore) GetOrCreateAccount(ctx context.Context, email, note string) (apiaccess.Account, error) {
	if a, err := m.GetAccountByEmail(ctx, email); err == nil {
		return a, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	a := apiaccess.Account{ID: m.id(), Email: apiaccess.NormalizeEmail(email), Status: "active", Note: note, CreatedAt: time.Now()}
	m.accounts[a.ID] = a
	return a, nil
}

func (m *memStore) SetAccountStatus(_ context.Context, id int64, status string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.accounts[id]
	if !ok {
		return apiaccess.ErrNotFound
	}
	a.Status = status
	m.accounts[id] = a
	return nil
}

func (m *memStore) SetAccountNote(_ context.Context, id int64, note string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.accounts[id]
	if !ok {
		return apiaccess.ErrNotFound
	}
	a.Note = note
	m.accounts[id] = a
	return nil
}

func (m *memStore) SetStripeCustomerID(_ context.Context, id int64, customerID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.accounts[id]
	if a.StripeCustomerID == "" {
		a.StripeCustomerID = customerID
		m.accounts[id] = a
	}
	return a.StripeCustomerID, nil
}

func (m *memStore) ListAccounts(context.Context) ([]apiaccess.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []apiaccess.Account
	for _, a := range m.accounts {
		out = append(out, a)
	}
	slices.SortFunc(out, func(a, b apiaccess.Account) int { return strings.Compare(a.Email, b.Email) })
	return out, nil
}

func (m *memStore) SearchAccounts(ctx context.Context, q string) ([]apiaccess.Account, error) {
	all, _ := m.ListAccounts(ctx)
	var out []apiaccess.Account
	for _, a := range all {
		if strings.Contains(a.Email, strings.ToLower(q)) {
			out = append(out, a)
		}
	}
	return out, nil
}

func (m *memStore) CreateMagicLink(_ context.Context, accountID int64, ttl time.Duration) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	token := fmt.Sprintf("tok%032d", m.id())
	m.links[apiaccess.HashKey(token)] = &memLink{accountID: accountID, expires: time.Now().Add(ttl)}
	return token, nil
}

func (m *memStore) ConsumeMagicLink(ctx context.Context, token string, now time.Time) (apiaccess.Account, error) {
	m.mu.Lock()
	l, ok := m.links[apiaccess.HashKey(token)]
	if !ok || l.used || !now.Before(l.expires) {
		m.mu.Unlock()
		return apiaccess.Account{}, apiaccess.ErrNotFound
	}
	l.used = true
	m.mu.Unlock()
	return m.GetAccount(ctx, l.accountID)
}

func (m *memStore) DeleteMagicLink(_ context.Context, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.links, apiaccess.HashKey(token))
	return nil
}

func (m *memStore) CreateKey(_ context.Context, accountID int64, label string, kind apiaccess.KeyKind) (string, apiaccess.Key, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	plain, hash, prefix, err := apiaccess.GenerateKey(kind)
	if err != nil {
		return "", apiaccess.Key{}, err
	}
	k := &apiaccess.Key{ID: m.id(), AccountID: accountID, Hash: hash, Prefix: prefix, Label: label, CreatedAt: time.Now()}
	m.keys[k.ID] = k
	return plain, *k, nil
}

func (m *memStore) ListKeys(_ context.Context, accountID int64) ([]apiaccess.Key, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listKeysErr != nil {
		return nil, m.listKeysErr
	}
	var out []apiaccess.Key
	for _, k := range m.keys {
		if k.AccountID == accountID {
			out = append(out, *k)
		}
	}
	slices.SortFunc(out, func(a, b apiaccess.Key) int { return int(a.ID - b.ID) })
	return out, nil
}

func (m *memStore) RevokeKey(_ context.Context, id, accountID int64) (apiaccess.Key, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.keys[id]
	if !ok || k.RevokedAt != nil || (accountID != 0 && k.AccountID != accountID) {
		return apiaccess.Key{}, apiaccess.ErrNotFound
	}
	now := time.Now()
	k.RevokedAt = &now
	return *k, nil
}

func (m *memStore) ListEntitlements(_ context.Context, accountID int64) ([]apiaccess.Entitlement, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listEntitlementsErr != nil {
		return nil, m.listEntitlementsErr
	}
	var out []apiaccess.Entitlement
	for _, e := range m.ents {
		if e.AccountID == accountID {
			out = append(out, *e)
		}
	}
	slices.SortFunc(out, func(a, b apiaccess.Entitlement) int { return int(a.ID - b.ID) })
	return out, nil
}

func (m *memStore) AddEntitlement(_ context.Context, e apiaccess.Entitlement) (apiaccess.Entitlement, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entitlementErr != nil {
		return apiaccess.Entitlement{}, m.entitlementErr
	}
	e.ID = m.id()
	if e.Status == "" {
		e.Status = "active"
	}
	if e.ValidFrom.IsZero() {
		// A fixed sentinel, not time.Now(), so it stays before any test's mocked clock.
		e.ValidFrom = time.Unix(0, 0)
	}
	m.ents[e.ID] = &e
	return e, nil
}

func (m *memStore) UpsertStripeEntitlement(ctx context.Context, e apiaccess.Entitlement) (apiaccess.Entitlement, error) {
	m.mu.Lock()
	for _, have := range m.ents {
		if have.ExternalRef == e.ExternalRef && e.ExternalRef != "" {
			e.ID = have.ID
			m.ents[e.ID] = &e
			m.mu.Unlock()
			return e, nil
		}
	}
	m.mu.Unlock()
	return m.AddEntitlement(ctx, e)
}

func (m *memStore) ListActiveStripeRefs(context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, e := range m.ents {
		if e.Source == "stripe" && e.Status == "active" {
			out = append(out, e.ExternalRef)
		}
	}
	return out, nil
}

func (m *memStore) EndEntitlement(_ context.Context, id int64, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.ents[id]
	if !ok {
		return apiaccess.ErrNotFound
	}
	e.Status = "ended"
	e.ValidUntil = &at
	return nil
}

func (m *memStore) SummarizeUsage(_ context.Context, since, until time.Time, accountID int64) ([]apiaccess.UsageRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.usageErr != nil {
		return nil, m.usageErr
	}
	var out []apiaccess.UsageRow
	for _, u := range m.usage {
		if (accountID == 0 || u.Row.AccountID == accountID) && !u.Ts.Before(since) && u.Ts.Before(until) {
			out = append(out, u.Row)
		}
	}
	return out, nil
}

func (m *memStore) CreateTrial(_ context.Context, email string, accountID int64, endsAt, notBefore time.Time) (apiaccess.Trial, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	email = apiaccess.NormalizeEmail(email)
	for _, t := range m.trials {
		if t.PatreonEmail == email && t.GrantedAt.After(notBefore) {
			return apiaccess.Trial{}, apiaccess.ErrTrialTooSoon
		}
	}
	t := &apiaccess.Trial{ID: m.id(), PatreonEmail: email, AccountID: accountID, GrantedAt: notBefore.Add(180 * 24 * time.Hour), EndsAt: endsAt}
	m.trials[t.ID] = t
	return *t, nil
}

func (m *memStore) DeleteTrial(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.trials, id)
	return nil
}

func (m *memStore) LastTrial(_ context.Context, email string) (apiaccess.Trial, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var last *apiaccess.Trial
	for _, t := range m.trials {
		if t.PatreonEmail == apiaccess.NormalizeEmail(email) && (last == nil || t.GrantedAt.After(last.GrantedAt)) {
			last = t
		}
	}
	if last == nil {
		return apiaccess.Trial{}, apiaccess.ErrNotFound
	}
	return *last, nil
}

func (m *memStore) TrialsToRemind(_ context.Context, from, to time.Time) ([]apiaccess.Trial, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []apiaccess.Trial
	for _, t := range m.trials {
		if t.ReminderSentAt == nil && !t.EndsAt.Before(from) && t.EndsAt.Before(to) {
			out = append(out, *t)
		}
	}
	return out, nil
}

func (m *memStore) MarkTrialReminded(_ context.Context, id int64, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.trials[id]
	if !ok {
		return apiaccess.ErrNotFound
	}
	t.ReminderSentAt = &at
	return nil
}

func (m *memStore) CreateInvite(_ context.Context, intervalKey, email string, ttl time.Duration, note string) (string, apiaccess.Invite, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	token := "inv" + strings.Repeat("y", 30)
	inv := apiaccess.Invite{TokenHash: apiaccess.HashKey(token), IntervalKey: intervalKey, Email: apiaccess.NormalizeEmail(email), ExpiresAt: time.Now().Add(ttl), Note: note}
	m.invites[inv.TokenHash] = inv
	return token, inv, nil
}

func (m *memStore) ConsumeInvite(_ context.Context, token, email string, now time.Time) (apiaccess.Invite, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, ok := m.invites[apiaccess.HashKey(token)]
	if !ok || inv.UsedAt != nil || !now.Before(inv.ExpiresAt) || (inv.Email != "" && inv.Email != apiaccess.NormalizeEmail(email)) {
		return apiaccess.Invite{}, apiaccess.ErrInviteInvalid
	}
	inv.UsedAt = &now
	m.invites[inv.TokenHash] = inv
	return inv, nil
}

func (m *memStore) ReleaseInvite(_ context.Context, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, ok := m.invites[apiaccess.HashKey(token)]
	if !ok {
		return nil
	}
	inv.UsedAt = nil
	m.invites[inv.TokenHash] = inv
	return nil
}

func (m *memStore) Notify(_ context.Context, payload string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notified = append(m.notified, payload)
	return nil
}

// RecordAdminAction appends one action; ListAdminActions reads them newest first.
func (m *memStore) RecordAdminAction(_ context.Context, actor, action string, accountID int64, target, detail string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.actions = append(m.actions, apiaccess.AdminAction{ID: m.id(), At: time.Now(), Actor: apiaccess.NormalizeEmail(actor), Action: action, AccountID: accountID, Target: target, Detail: detail})
	return nil
}

func (m *memStore) ListAdminActions(_ context.Context, accountID int64, limit int) ([]apiaccess.AdminAction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listActionsErr != nil {
		return nil, m.listActionsErr
	}
	var out []apiaccess.AdminAction
	for i := len(m.actions) - 1; i >= 0 && len(out) < limit; i-- {
		a := m.actions[i]
		if accountID != 0 && a.AccountID != accountID {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

// ListDemoAccess mirrors the store query: active trial and manual rows, newest first.
func (m *memStore) ListDemoAccess(context.Context) ([]apiaccess.DemoAccess, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.nowOr()
	var out []apiaccess.DemoAccess
	for _, e := range m.ents {
		if e.Status != "active" || (e.Source != "trial" && e.Source != "manual") {
			continue
		}
		if e.ValidUntil != nil && !e.ValidUntil.After(now) {
			continue
		}
		d := apiaccess.DemoAccess{AccountID: e.AccountID, Email: m.accounts[e.AccountID].Email,
			Source: e.Source, Note: e.Note, GrantedAt: e.ValidFrom, EndsAt: e.ValidUntil}
		if e.Source == "trial" {
			var latest *apiaccess.Trial
			for _, t := range m.trials {
				if t.AccountID == e.AccountID && (latest == nil || t.GrantedAt.After(latest.GrantedAt)) {
					latest = t
				}
			}
			if latest != nil {
				d.Requester = latest.PatreonEmail
			}
		}
		for _, k := range m.keys {
			if k.AccountID != e.AccountID || k.RevokedAt != nil {
				continue
			}
			d.Keys++
			if k.LastUsedAt != nil && (d.LastUsed == nil || k.LastUsedAt.After(*d.LastUsed)) {
				last := *k.LastUsedAt
				d.LastUsed = &last
			}
		}
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b apiaccess.DemoAccess) int { return b.GrantedAt.Compare(a.GrantedAt) })
	return out, nil
}

// ConsumeNonce records nonce until expiresAt; a second call inside that window fails.
func (m *memStore) ConsumeNonce(_ context.Context, nonce string, expiresAt, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for n, exp := range m.nonces {
		if exp.Before(now) {
			delete(m.nonces, n)
		}
	}
	if _, ok := m.nonces[nonce]; ok {
		return apiaccess.ErrNonceUsed
	}
	m.nonces[nonce] = expiresAt
	return nil
}
