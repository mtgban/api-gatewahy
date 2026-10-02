// Package apiaccesstest is an in-memory apiaccess store for tests, following
// the client's SQL; apiaccess's contract test runs one table against both.
package apiaccesstest

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
)

// eventReclaimAfter mirrors apiaccess: how long an unfinished claim blocks a retry.
const eventReclaimAfter = 5 * time.Minute

// createKeyAttempts mirrors apiaccess: the first insert plus three regenerations.
const createKeyAttempts = 4

// errUnique stands in for the Postgres unique violation the client would return.
var errUnique = errors.New("apiaccesstest: duplicate key value violates unique constraint")

// errForeignKey stands in for the Postgres foreign key violation.
var errForeignKey = errors.New("apiaccesstest: insert violates foreign key constraint")

// MemStore is the apiaccess store in memory, safe for concurrent use.
type MemStore struct {
	// Now is the store's clock; New sets it to time.Now.
	Now func() time.Time
	// Fail maps a method name to the error it returns on entry.
	Fail map[string]error
	// FailAt limits Fail[name] to the call with that 1-based index.
	FailAt map[string]int
	// FailFrom limits Fail[name] to calls from that 1-based index on.
	FailFrom map[string]int
	// FailID limits Fail[name] to calls about that id, the method's first id argument.
	FailID map[string]int64
	// Calls counts entries per method name, failed ones included.
	Calls map[string]int
	// Notified holds every Notify payload, in order.
	Notified []string

	mu       sync.Mutex
	accounts []*apiaccess.Account
	keys     []*apiaccess.Key
	ents     []*apiaccess.Entitlement
	usage    []apiaccess.Usage
	trials   []*apiaccess.Trial
	invites  map[string]*apiaccess.Invite
	links    map[string]*magicLink
	events   map[string]*stripeEvent
	nonces   map[string]time.Time
	actions  []apiaccess.AdminAction
	seq      map[string]int64
}

type magicLink struct {
	accountID int64
	expiresAt time.Time
	used      bool
}

type stripeEvent struct {
	receivedAt time.Time
	processed  bool
}

// New returns an empty store on the wall clock.
func New() *MemStore {
	return &MemStore{
		Now:      time.Now,
		Fail:     map[string]error{},
		FailAt:   map[string]int{},
		FailFrom: map[string]int{},
		FailID:   map[string]int64{},
		Calls:    map[string]int{},
		invites:  map[string]*apiaccess.Invite{},
		links:    map[string]*magicLink{},
		events:   map[string]*stripeEvent{},
		nonces:   map[string]time.Time{},
		seq:      map[string]int64{},
	}
}

// CreateTrialEntitlement names the entitlement insert inside CreateTrial in Fail and Calls.
const CreateTrialEntitlement = "CreateTrial.entitlement"

// enter counts the call and returns its injected failure; the caller holds mu.
func (m *MemStore) enter(method string) error {
	return m.enterID(method, 0)
}

// enterID is enter for a method whose first id argument is id, so FailID can aim at it.
func (m *MemStore) enterID(method string, id int64) error {
	if m.Calls == nil {
		m.Calls = map[string]int{}
	}
	m.Calls[method]++
	n := m.Calls[method]
	if at, ok := m.FailAt[method]; ok && at != n {
		return nil
	}
	if from, ok := m.FailFrom[method]; ok && n < from {
		return nil
	}
	if want, ok := m.FailID[method]; ok && want != id {
		return nil
	}
	return m.Fail[method]
}

// now reads the clock at the microsecond precision Postgres stores.
func (m *MemStore) now() time.Time {
	return pgTime(m.Now())
}

func pgTime(t time.Time) time.Time {
	return t.Round(time.Microsecond)
}

func pgTimePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := pgTime(*t)
	return &v
}

// nextID is the table's identity sequence.
func (m *MemStore) nextID(table string) int64 {
	m.seq[table]++
	return m.seq[table]
}

func (m *MemStore) account(id int64) *apiaccess.Account {
	for _, a := range m.accounts {
		if a.ID == id {
			return a
		}
	}
	return nil
}

func (m *MemStore) accountByEmail(email string) *apiaccess.Account {
	email = apiaccess.NormalizeEmail(email)
	for _, a := range m.accounts {
		if a.Email == email {
			return a
		}
	}
	return nil
}

// CreateAccount inserts an active account. A duplicate email is an error.
func (m *MemStore) CreateAccount(_ context.Context, email, note string) (apiaccess.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enter("CreateAccount"); err != nil {
		return apiaccess.Account{}, err
	}
	if m.accountByEmail(email) != nil {
		return apiaccess.Account{}, errUnique
	}
	return *m.insertAccount(email, note), nil
}

func (m *MemStore) insertAccount(email, note string) *apiaccess.Account {
	a := &apiaccess.Account{ID: m.nextID("accounts"), Email: apiaccess.NormalizeEmail(email), Status: apiaccess.AccountActive, CreatedAt: m.now(), Note: note}
	m.accounts = append(m.accounts, a)
	return a
}

// GetAccountByEmail returns the account with that email, or ErrNotFound.
func (m *MemStore) GetAccountByEmail(_ context.Context, email string) (apiaccess.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enter("GetAccountByEmail"); err != nil {
		return apiaccess.Account{}, err
	}
	a := m.accountByEmail(email)
	if a == nil {
		return apiaccess.Account{}, apiaccess.ErrNotFound
	}
	return *a, nil
}

// GetAccount returns the account with that id, or ErrNotFound.
func (m *MemStore) GetAccount(_ context.Context, id int64) (apiaccess.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enterID("GetAccount", id); err != nil {
		return apiaccess.Account{}, err
	}
	a := m.account(id)
	if a == nil {
		return apiaccess.Account{}, apiaccess.ErrNotFound
	}
	return *a, nil
}

// SetAccountStatus sets AccountActive or AccountSuspended.
func (m *MemStore) SetAccountStatus(_ context.Context, id int64, status apiaccess.AccountStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enterID("SetAccountStatus", id); err != nil {
		return err
	}
	if !status.Valid() {
		return errors.New("apiaccess: status must be active or suspended")
	}
	a := m.account(id)
	if a == nil {
		return apiaccess.ErrNotFound
	}
	a.Status = status
	return nil
}

// SetStripeCustomerID records the customer once; a later call returns the first id.
func (m *MemStore) SetStripeCustomerID(_ context.Context, accountID int64, customerID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enterID("SetStripeCustomerID", accountID); err != nil {
		return "", err
	}
	a := m.account(accountID)
	if a == nil {
		return "", apiaccess.ErrNotFound
	}
	if a.StripeCustomerID != "" {
		return a.StripeCustomerID, nil
	}
	for _, other := range m.accounts {
		if customerID != "" && other.StripeCustomerID == customerID {
			return "", errUnique
		}
	}
	a.StripeCustomerID = customerID
	return customerID, nil
}

// GetAccountByStripeCustomer returns the account that owns the customer id.
func (m *MemStore) GetAccountByStripeCustomer(_ context.Context, customerID string) (apiaccess.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enter("GetAccountByStripeCustomer"); err != nil {
		return apiaccess.Account{}, err
	}
	for _, a := range m.accounts {
		// An account without a customer holds NULL, which equals nothing.
		if a.StripeCustomerID != "" && a.StripeCustomerID == customerID {
			return *a, nil
		}
	}
	return apiaccess.Account{}, apiaccess.ErrNotFound
}

// GetOrCreateAccount returns the account for email, creating an active one
// with note when none exists. An existing row keeps its own note.
func (m *MemStore) GetOrCreateAccount(_ context.Context, email, note string) (apiaccess.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enter("GetOrCreateAccount"); err != nil {
		return apiaccess.Account{}, err
	}
	if a := m.accountByEmail(email); a != nil {
		return *a, nil
	}
	return *m.insertAccount(email, note), nil
}

// SetAccountNote replaces the operator note.
func (m *MemStore) SetAccountNote(_ context.Context, id int64, note string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enterID("SetAccountNote", id); err != nil {
		return err
	}
	a := m.account(id)
	if a == nil {
		return apiaccess.ErrNotFound
	}
	a.Note = note
	return nil
}

// SearchAccounts lists accounts whose email contains q, case-insensitively.
func (m *MemStore) SearchAccounts(_ context.Context, q string) ([]apiaccess.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enter("SearchAccounts"); err != nil {
		return nil, err
	}
	q = strings.ToLower(strings.TrimSpace(q))
	var out []apiaccess.Account
	for _, a := range m.accounts {
		if strings.Contains(strings.ToLower(a.Email), q) {
			out = append(out, *a)
		}
	}
	slices.SortFunc(out, func(a, b apiaccess.Account) int { return strings.Compare(a.Email, b.Email) })
	if len(out) > 200 {
		out = out[:200]
	}
	return out, nil
}

// ListAccounts returns every account, oldest first.
func (m *MemStore) ListAccounts(context.Context) ([]apiaccess.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enter("ListAccounts"); err != nil {
		return nil, err
	}
	var out []apiaccess.Account
	for _, a := range m.accounts {
		out = append(out, *a)
	}
	return out, nil
}

// BumpSessionEpoch signs out every portal session of the account and returns the new epoch.
func (m *MemStore) BumpSessionEpoch(_ context.Context, accountID int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enterID("BumpSessionEpoch", accountID); err != nil {
		return 0, err
	}
	a := m.account(accountID)
	if a == nil {
		return 0, apiaccess.ErrNotFound
	}
	a.SessionEpoch++
	return a.SessionEpoch, nil
}

func copyKey(k *apiaccess.Key) apiaccess.Key {
	out := *k
	out.LastUsedAt = pgTimePtr(k.LastUsedAt)
	out.RevokedAt = pgTimePtr(k.RevokedAt)
	return out
}

// CreateKey mints a key of kind for the account and returns the plaintext once.
// A prefix a live key already holds is regenerated.
func (m *MemStore) CreateKey(_ context.Context, accountID int64, label string, kind apiaccess.KeyKind) (string, apiaccess.Key, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enterID("CreateKey", accountID); err != nil {
		return "", apiaccess.Key{}, err
	}
	for attempt := 0; attempt < createKeyAttempts; attempt++ {
		plaintext, hash, prefix, err := apiaccess.GenerateKey(kind)
		if err != nil {
			return "", apiaccess.Key{}, err
		}
		if m.account(accountID) == nil {
			return "", apiaccess.Key{}, errForeignKey
		}
		if slices.ContainsFunc(m.keys, func(k *apiaccess.Key) bool { return k.Prefix == prefix && k.RevokedAt == nil }) {
			continue
		}
		k := &apiaccess.Key{ID: m.nextID("api_keys"), AccountID: accountID, Hash: hash, Prefix: prefix, Label: label, Kind: kind, CreatedAt: m.now()}
		m.keys = append(m.keys, k)
		return plaintext, copyKey(k), nil
	}
	return "", apiaccess.Key{}, errUnique
}

// RevokeKey revokes key id. accountID 0 means any account; otherwise the key must belong to it.
func (m *MemStore) RevokeKey(_ context.Context, id, accountID int64) (apiaccess.Key, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enterID("RevokeKey", id); err != nil {
		return apiaccess.Key{}, err
	}
	for _, k := range m.keys {
		if k.ID == id && k.RevokedAt == nil && (accountID == 0 || k.AccountID == accountID) {
			now := m.now()
			k.RevokedAt = &now
			return copyKey(k), nil
		}
	}
	return apiaccess.Key{}, apiaccess.ErrNotFound
}

// RevokeKeyByPrefix revokes the one live key whose prefix matches.
func (m *MemStore) RevokeKeyByPrefix(_ context.Context, prefix string) (apiaccess.Key, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enter("RevokeKeyByPrefix"); err != nil {
		return apiaccess.Key{}, err
	}
	var matches []*apiaccess.Key
	for _, k := range m.keys {
		if k.Prefix == prefix && k.RevokedAt == nil {
			matches = append(matches, k)
		}
	}
	switch len(matches) {
	case 0:
		return apiaccess.Key{}, apiaccess.ErrNotFound
	case 1:
	default:
		return apiaccess.Key{}, fmt.Errorf("apiaccess: prefix %q matches %d live keys", prefix, len(matches))
	}
	now := m.now()
	matches[0].RevokedAt = &now
	return copyKey(matches[0]), nil
}

// ListKeys returns the account's keys, oldest first, revoked included.
func (m *MemStore) ListKeys(_ context.Context, accountID int64) ([]apiaccess.Key, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enterID("ListKeys", accountID); err != nil {
		return nil, err
	}
	var out []apiaccess.Key
	for _, k := range m.keys {
		if k.AccountID == accountID {
			out = append(out, copyKey(k))
		}
	}
	return out, nil
}

func copyEntitlement(e *apiaccess.Entitlement) apiaccess.Entitlement {
	out := *e
	out.Games = slices.Clone(e.Games)
	out.Modes = slices.Clone(e.Modes)
	out.Addons = slices.Clone(e.Addons)
	out.ValidUntil = pgTimePtr(e.ValidUntil)
	return out
}

// prepare canonicalizes e, then applies the insert's column constraints.
func (m *MemStore) prepare(e apiaccess.Entitlement) (apiaccess.Entitlement, error) {
	e, err := m.canonical(e)
	if err != nil {
		return apiaccess.Entitlement{}, err
	}
	return e, m.checkColumns(e)
}

// canonical canonicalizes and validates e as the client does before any SQL runs.
func (m *MemStore) canonical(e apiaccess.Entitlement) (apiaccess.Entitlement, error) {
	scope, err := apiaccess.ValidateStoreScope(e.StoreScope)
	if err != nil {
		return apiaccess.Entitlement{}, err
	}
	e.StoreScope = scope
	if e.Modes, err = apiaccess.ValidateModes(e.Modes); err != nil {
		return apiaccess.Entitlement{}, err
	}
	if e.Status == "" {
		e.Status = apiaccess.EntitlementActive
	}
	if e.ValidFrom.IsZero() {
		e.ValidFrom = m.Now()
	}
	e.ValidFrom = pgTime(e.ValidFrom)
	if e.Addons == nil {
		e.Addons = []string{}
	}
	e.Games = slices.Clone(e.Games)
	e.Addons = slices.Clone(e.Addons)
	e.ValidUntil = pgTimePtr(e.ValidUntil)
	return e, nil
}

// checkColumns applies the constraints Postgres checks on insert.
func (m *MemStore) checkColumns(e apiaccess.Entitlement) error {
	// games is text[] NOT NULL, and a nil slice is sent as NULL.
	if e.Games == nil {
		return errors.New("apiaccesstest: null value in column games violates not-null constraint")
	}
	if m.account(e.AccountID) == nil {
		return errForeignKey
	}
	return nil
}

func (m *MemStore) entByRef(ref string) *apiaccess.Entitlement {
	if ref == "" {
		return nil
	}
	for _, e := range m.ents {
		if e.ExternalRef == ref {
			return e
		}
	}
	return nil
}

// AddEntitlement canonicalizes and validates e, then inserts it.
func (m *MemStore) AddEntitlement(_ context.Context, e apiaccess.Entitlement) (apiaccess.Entitlement, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enter("AddEntitlement"); err != nil {
		return apiaccess.Entitlement{}, err
	}
	e, err := m.prepare(e)
	if err != nil {
		return apiaccess.Entitlement{}, err
	}
	if m.entByRef(e.ExternalRef) != nil {
		return apiaccess.Entitlement{}, errUnique
	}
	return m.insertEntitlement(e), nil
}

func (m *MemStore) insertEntitlement(e apiaccess.Entitlement) apiaccess.Entitlement {
	e.ID = m.nextID("entitlements")
	e.CreatedAt = m.now()
	m.ents = append(m.ents, &e)
	return copyEntitlement(&e)
}

// UpsertStripeEntitlement inserts or updates the one row for e.ExternalRef.
// valid_from is kept from the first insert; everything else follows e.
func (m *MemStore) UpsertStripeEntitlement(_ context.Context, e apiaccess.Entitlement) (apiaccess.Entitlement, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enter("UpsertStripeEntitlement"); err != nil {
		return apiaccess.Entitlement{}, err
	}
	if e.ExternalRef == "" {
		return apiaccess.Entitlement{}, errors.New("apiaccess: external_ref is required")
	}
	e, err := m.prepare(e)
	if err != nil {
		return apiaccess.Entitlement{}, err
	}
	have := m.entByRef(e.ExternalRef)
	if have == nil {
		return m.insertEntitlement(e), nil
	}
	have.AccountID, have.Source, have.Games = e.AccountID, e.Source, e.Games
	have.StoreScope, have.Modes, have.Addons = e.StoreScope, e.Modes, e.Addons
	have.Status, have.ValidUntil, have.Note = e.Status, e.ValidUntil, e.Note
	return copyEntitlement(have), nil
}

// ListActiveStripeRefs returns every active stripe row, ordered by account and subscription id.
func (m *MemStore) ListActiveStripeRefs(context.Context) ([]apiaccess.StripeRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enter("ListActiveStripeRefs"); err != nil {
		return nil, err
	}
	var out []apiaccess.StripeRef
	for _, e := range m.ents {
		if e.Source == apiaccess.SourceStripe && e.Status == apiaccess.EntitlementActive && e.ExternalRef != "" {
			out = append(out, apiaccess.StripeRef{AccountID: e.AccountID, SubID: e.ExternalRef})
		}
	}
	slices.SortFunc(out, func(a, b apiaccess.StripeRef) int {
		return cmp.Or(cmp.Compare(a.AccountID, b.AccountID), strings.Compare(a.SubID, b.SubID))
	})
	return out, nil
}

// EndEntitlement marks the row ended as of at and returns it. accountID 0 means
// any account. A stripe row is refused, before an already-ended one is.
func (m *MemStore) EndEntitlement(_ context.Context, id, accountID int64, at time.Time) (apiaccess.Entitlement, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enterID("EndEntitlement", id); err != nil {
		return apiaccess.Entitlement{}, err
	}
	for _, e := range m.ents {
		if e.ID != id || (accountID != 0 && e.AccountID != accountID) {
			continue
		}
		if e.Source == apiaccess.SourceStripe {
			return apiaccess.Entitlement{}, apiaccess.ErrStripeEntitlement
		}
		if e.Status == apiaccess.EntitlementEnded {
			return apiaccess.Entitlement{}, apiaccess.ErrNotFound
		}
		e.Status = apiaccess.EntitlementEnded
		e.ValidUntil = pgTimePtr(&at)
		return copyEntitlement(e), nil
	}
	return apiaccess.Entitlement{}, apiaccess.ErrNotFound
}

// ListEntitlements returns every row for the account, oldest first.
func (m *MemStore) ListEntitlements(_ context.Context, accountID int64) ([]apiaccess.Entitlement, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enterID("ListEntitlements", accountID); err != nil {
		return nil, err
	}
	out := []apiaccess.Entitlement{}
	for _, e := range m.ents {
		if e.AccountID == accountID {
			out = append(out, copyEntitlement(e))
		}
	}
	return out, nil
}

// InsertUsage writes rows in one batch.
func (m *MemStore) InsertUsage(_ context.Context, rows []apiaccess.Usage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enter("InsertUsage"); err != nil {
		return err
	}
	for _, u := range rows {
		u.Ts = pgTime(u.Ts)
		m.usage = append(m.usage, u)
	}
	return nil
}

func inWindow(ts, since, until time.Time) bool {
	return !ts.Before(since) && ts.Before(until)
}

func errorCount(status int) int64 {
	if status >= 400 {
		return 1
	}
	return 0
}

// SummarizeUsage groups requests by account and game within [since, until).
func (m *MemStore) SummarizeUsage(_ context.Context, since, until time.Time, accountID int64) ([]apiaccess.UsageRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enter("SummarizeUsage"); err != nil {
		return nil, err
	}
	type group struct {
		accountID int64
		game      string
	}
	rows := map[group]*apiaccess.UsageRow{}
	for _, u := range m.usage {
		a := m.account(u.AccountID)
		if a == nil || !inWindow(u.Ts, since, until) || (accountID != 0 && u.AccountID != accountID) {
			continue
		}
		g := group{u.AccountID, u.Game}
		r := rows[g]
		if r == nil {
			r = &apiaccess.UsageRow{AccountID: u.AccountID, Email: a.Email, Game: u.Game}
			rows[g] = r
		}
		r.Requests++
		r.Bytes += u.Bytes
		r.Errors += errorCount(u.Status)
	}
	var out []apiaccess.UsageRow
	for _, r := range rows {
		out = append(out, *r)
	}
	slices.SortFunc(out, func(a, b apiaccess.UsageRow) int {
		return cmp.Or(strings.Compare(a.Email, b.Email), strings.Compare(a.Game, b.Game))
	})
	return out, nil
}

// UsageByKey summarizes usage per key per UTC day; accountID 0 means every account.
func (m *MemStore) UsageByKey(_ context.Context, since, until time.Time, accountID int64) ([]apiaccess.KeyUsageRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enter("UsageByKey"); err != nil {
		return nil, err
	}
	type group struct {
		keyID int64
		day   time.Time
	}
	rows := map[group]*apiaccess.KeyUsageRow{}
	for _, u := range m.usage {
		if !inWindow(u.Ts, since, until) || (accountID != 0 && u.AccountID != accountID) {
			continue
		}
		var k *apiaccess.Key
		for _, have := range m.keys {
			if have.ID == u.KeyID {
				k = have
			}
		}
		if k == nil {
			continue
		}
		ts := u.Ts.UTC()
		g := group{k.ID, time.Date(ts.Year(), ts.Month(), ts.Day(), 0, 0, 0, 0, time.UTC)}
		r := rows[g]
		if r == nil {
			r = &apiaccess.KeyUsageRow{KeyID: k.ID, Prefix: k.Prefix, Label: k.Label, Day: g.day}
			rows[g] = r
		}
		r.Requests++
		r.Bytes += u.Bytes
		r.Errors += errorCount(u.Status)
	}
	var out []apiaccess.KeyUsageRow
	for _, r := range rows {
		out = append(out, *r)
	}
	slices.SortFunc(out, func(a, b apiaccess.KeyUsageRow) int {
		return cmp.Or(cmp.Compare(a.KeyID, b.KeyID), a.Day.Compare(b.Day))
	})
	return out, nil
}

// TopPaths lists the paths one key requested most, up to limit.
func (m *MemStore) TopPaths(_ context.Context, since, until time.Time, keyID int64, limit int) ([]apiaccess.PathUsageRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enter("TopPaths"); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 10
	}
	rows := map[string]*apiaccess.PathUsageRow{}
	for _, u := range m.usage {
		if u.KeyID != keyID || !inWindow(u.Ts, since, until) {
			continue
		}
		r := rows[u.Path]
		if r == nil {
			r = &apiaccess.PathUsageRow{Path: u.Path}
			rows[u.Path] = r
		}
		r.Requests++
		r.Errors += errorCount(u.Status)
	}
	var out []apiaccess.PathUsageRow
	for _, r := range rows {
		out = append(out, *r)
	}
	slices.SortFunc(out, func(a, b apiaccess.PathUsageRow) int {
		return cmp.Or(cmp.Compare(b.Requests, a.Requests), strings.Compare(a.Path, b.Path))
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func copyTrial(t *apiaccess.Trial) apiaccess.Trial {
	out := *t
	out.ReminderSentAt = pgTimePtr(t.ReminderSentAt)
	return out
}

// CreateTrial records a trial for ent's account, unless one for email was
// granted after notBefore, and inserts ent ending at endsAt: both or neither.
func (m *MemStore) CreateTrial(_ context.Context, email string, endsAt, notBefore time.Time, ent apiaccess.Entitlement) (apiaccess.Trial, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enter("CreateTrial"); err != nil {
		return apiaccess.Trial{}, err
	}
	email = apiaccess.NormalizeEmail(email)
	ent.ValidUntil = &endsAt
	ent, err := m.canonical(ent)
	if err != nil {
		return apiaccess.Trial{}, err
	}
	for _, t := range m.trials {
		if t.PatreonEmail == email && t.GrantedAt.After(notBefore) {
			return apiaccess.Trial{}, apiaccess.ErrTrialTooSoon
		}
	}
	if m.account(ent.AccountID) == nil {
		return apiaccess.Trial{}, errForeignKey
	}
	if err := m.enter(CreateTrialEntitlement); err != nil {
		return apiaccess.Trial{}, err
	}
	if err := m.checkColumns(ent); err != nil {
		return apiaccess.Trial{}, err
	}
	if m.entByRef(ent.ExternalRef) != nil {
		return apiaccess.Trial{}, errUnique
	}
	t := &apiaccess.Trial{ID: m.nextID("trials"), PatreonEmail: email, AccountID: ent.AccountID, GrantedAt: m.now(), EndsAt: pgTime(endsAt)}
	m.trials = append(m.trials, t)
	m.insertEntitlement(ent)
	return copyTrial(t), nil
}

// LastTrial is the most recent trial for email, or ErrNotFound.
func (m *MemStore) LastTrial(_ context.Context, email string) (apiaccess.Trial, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enter("LastTrial"); err != nil {
		return apiaccess.Trial{}, err
	}
	email = apiaccess.NormalizeEmail(email)
	var last *apiaccess.Trial
	for _, t := range m.trials {
		if t.PatreonEmail == email && (last == nil || !t.GrantedAt.Before(last.GrantedAt)) {
			last = t
		}
	}
	if last == nil {
		return apiaccess.Trial{}, apiaccess.ErrNotFound
	}
	return copyTrial(last), nil
}

// TrialsToRemind lists trials ending in [from, to) that were not reminded yet.
func (m *MemStore) TrialsToRemind(_ context.Context, from, to time.Time) ([]apiaccess.Trial, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enter("TrialsToRemind"); err != nil {
		return nil, err
	}
	var out []apiaccess.Trial
	for _, t := range m.trials {
		if t.ReminderSentAt == nil && inWindow(t.EndsAt, from, to) {
			out = append(out, copyTrial(t))
		}
	}
	slices.SortStableFunc(out, func(a, b apiaccess.Trial) int { return a.EndsAt.Compare(b.EndsAt) })
	return out, nil
}

// MarkTrialReminded records that the ending-soon mail went out.
func (m *MemStore) MarkTrialReminded(_ context.Context, id int64, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enterID("MarkTrialReminded", id); err != nil {
		return err
	}
	for _, t := range m.trials {
		if t.ID == id {
			t.ReminderSentAt = pgTimePtr(&at)
			return nil
		}
	}
	return apiaccess.ErrNotFound
}

// newToken is the 32 random bytes the client encodes as an invite or link token.
func newToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func copyInvite(inv *apiaccess.Invite) apiaccess.Invite {
	out := *inv
	out.UsedAt = pgTimePtr(inv.UsedAt)
	return out
}

// CreateInvite mints a token, stores its hash, and returns the token once.
// An empty email leaves the invite usable by any account.
func (m *MemStore) CreateInvite(_ context.Context, intervalKey, email string, ttl time.Duration, note string) (string, apiaccess.Invite, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enter("CreateInvite"); err != nil {
		return "", apiaccess.Invite{}, err
	}
	token, err := newToken()
	if err != nil {
		return "", apiaccess.Invite{}, err
	}
	inv := &apiaccess.Invite{TokenHash: apiaccess.HashKey(token), IntervalKey: intervalKey, Email: apiaccess.NormalizeEmail(email),
		ExpiresAt: pgTime(m.Now().Add(ttl)), CreatedAt: m.now(), Note: note}
	m.invites[inv.TokenHash] = inv
	return token, copyInvite(inv), nil
}

// ConsumeInvite marks the invite used if it is live and bound to email or to no one.
func (m *MemStore) ConsumeInvite(_ context.Context, token, email string, now time.Time) (apiaccess.Invite, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enter("ConsumeInvite"); err != nil {
		return apiaccess.Invite{}, err
	}
	inv, ok := m.invites[apiaccess.HashKey(token)]
	if !ok || inv.UsedAt != nil || !inv.ExpiresAt.After(now) || (inv.Email != "" && inv.Email != apiaccess.NormalizeEmail(email)) {
		return apiaccess.Invite{}, apiaccess.ErrInviteInvalid
	}
	inv.UsedAt = pgTimePtr(&now)
	return copyInvite(inv), nil
}

// ReleaseInvite clears used_at so a checkout that failed to start can retry.
func (m *MemStore) ReleaseInvite(_ context.Context, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enter("ReleaseInvite"); err != nil {
		return err
	}
	if inv, ok := m.invites[apiaccess.HashKey(token)]; ok {
		inv.UsedAt = nil
	}
	return nil
}

// InviteByToken returns the stored invite for a plaintext token. The client
// has no such read; tests use it to see whether an invite is spent.
func (m *MemStore) InviteByToken(token string) (apiaccess.Invite, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, ok := m.invites[apiaccess.HashKey(token)]
	if !ok {
		return apiaccess.Invite{}, false
	}
	return copyInvite(inv), true
}

// CreateMagicLink mints a one-time sign-in token for the account and stores its hash.
func (m *MemStore) CreateMagicLink(_ context.Context, accountID int64, ttl time.Duration) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enterID("CreateMagicLink", accountID); err != nil {
		return "", err
	}
	now := m.Now()
	for hash, l := range m.links {
		if l.expiresAt.Before(now) {
			delete(m.links, hash)
		}
	}
	token, err := newToken()
	if err != nil {
		return "", err
	}
	if m.account(accountID) == nil {
		return "", errForeignKey
	}
	m.links[apiaccess.HashKey(token)] = &magicLink{accountID: accountID, expiresAt: pgTime(now.Add(ttl))}
	return token, nil
}

// ConsumeMagicLink marks a live token used and returns its account, or ErrNotFound.
func (m *MemStore) ConsumeMagicLink(_ context.Context, token string, now time.Time) (apiaccess.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enter("ConsumeMagicLink"); err != nil {
		return apiaccess.Account{}, err
	}
	l, ok := m.links[apiaccess.HashKey(token)]
	if !ok || l.used || !l.expiresAt.After(now) {
		return apiaccess.Account{}, apiaccess.ErrNotFound
	}
	l.used = true
	a := m.account(l.accountID)
	if a == nil {
		return apiaccess.Account{}, apiaccess.ErrNotFound
	}
	return *a, nil
}

// DeleteMagicLink removes a token whose mail never went out.
func (m *MemStore) DeleteMagicLink(_ context.Context, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enter("DeleteMagicLink"); err != nil {
		return err
	}
	delete(m.links, apiaccess.HashKey(token))
	return nil
}

// BeginStripeEvent claims the event. False means it was handled already or
// is being handled right now.
func (m *MemStore) BeginStripeEvent(_ context.Context, id, _ string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enter("BeginStripeEvent"); err != nil {
		return false, err
	}
	now := m.now()
	ev, ok := m.events[id]
	if !ok {
		m.events[id] = &stripeEvent{receivedAt: now}
		return true, nil
	}
	if ev.processed || !ev.receivedAt.Before(now.Add(-eventReclaimAfter)) {
		return false, nil
	}
	ev.receivedAt = now
	return true, nil
}

// FinishStripeEvent marks the event processed.
func (m *MemStore) FinishStripeEvent(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enter("FinishStripeEvent"); err != nil {
		return err
	}
	if ev, ok := m.events[id]; ok {
		ev.processed = true
	}
	return nil
}

// DeleteStripeEvent drops the claim so Stripe's retry is handled afresh.
func (m *MemStore) DeleteStripeEvent(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enter("DeleteStripeEvent"); err != nil {
		return err
	}
	delete(m.events, id)
	return nil
}

// ConsumeNonce records nonce until expiresAt; a second call inside that window fails.
func (m *MemStore) ConsumeNonce(_ context.Context, nonce string, expiresAt, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enter("ConsumeNonce"); err != nil {
		return err
	}
	for n, exp := range m.nonces {
		if exp.Before(now) {
			delete(m.nonces, n)
		}
	}
	if _, ok := m.nonces[nonce]; ok {
		return apiaccess.ErrNonceUsed
	}
	m.nonces[nonce] = pgTime(expiresAt)
	return nil
}

// Notify records payload in Notified.
func (m *MemStore) Notify(_ context.Context, payload string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enter("Notify"); err != nil {
		return err
	}
	m.Notified = append(m.Notified, payload)
	return nil
}

// RecordAdminAction writes one audit row. accountID 0 means no account.
func (m *MemStore) RecordAdminAction(_ context.Context, actor, action string, accountID int64, target, detail string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enter("RecordAdminAction"); err != nil {
		return err
	}
	m.actions = append(m.actions, apiaccess.AdminAction{ID: m.nextID("admin_actions"), At: m.now(), Actor: apiaccess.NormalizeEmail(actor),
		Action: action, AccountID: accountID, Target: target, Detail: detail})
	return nil
}

// ListAdminActions returns the newest actions, for one account or for all when accountID is 0.
func (m *MemStore) ListAdminActions(_ context.Context, accountID int64, limit int) ([]apiaccess.AdminAction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enterID("ListAdminActions", accountID); err != nil {
		return nil, err
	}
	if limit < 0 {
		return nil, errors.New("apiaccesstest: LIMIT must not be negative")
	}
	var out []apiaccess.AdminAction
	for _, a := range m.actions {
		if accountID == 0 || a.AccountID == accountID {
			out = append(out, a)
		}
	}
	slices.SortFunc(out, func(a, b apiaccess.AdminAction) int {
		return cmp.Or(b.At.Compare(a.At), cmp.Compare(b.ID, a.ID))
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ListDemoAccess returns active trial and manual entitlements, newest first.
// TouchKeys is not implemented here, so LastUsed is always nil.
func (m *MemStore) ListDemoAccess(context.Context) ([]apiaccess.DemoAccess, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.enter("ListDemoAccess"); err != nil {
		return nil, err
	}
	now := m.Now()
	var out []apiaccess.DemoAccess
	for _, e := range m.ents {
		if e.Status != apiaccess.EntitlementActive || (e.Source != apiaccess.SourceTrial && e.Source != apiaccess.SourceManual) || (e.ValidUntil != nil && !e.ValidUntil.After(now)) {
			continue
		}
		a := m.account(e.AccountID)
		if a == nil {
			continue
		}
		d := apiaccess.DemoAccess{AccountID: e.AccountID, Email: a.Email, Source: e.Source, Note: e.Note, GrantedAt: e.ValidFrom, EndsAt: pgTimePtr(e.ValidUntil)}
		if e.Source == apiaccess.SourceTrial {
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
			if k.AccountID == e.AccountID && k.RevokedAt == nil {
				d.Keys++
			}
		}
		out = append(out, d)
	}
	slices.SortStableFunc(out, func(a, b apiaccess.DemoAccess) int { return b.GrantedAt.Compare(a.GrantedAt) })
	return out, nil
}
