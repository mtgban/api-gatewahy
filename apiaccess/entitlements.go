package apiaccess

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/lib/pq"
)

// Store scope presets the backend understands. DEV_ACCESS is never issued.
const (
	ScopeAll  = "ALL_ACCESS"
	ScopeBase = "BASE_ACCESS"
)

// ValidModes in canonical order.
var ValidModes = []string{"retail", "buylist", "sealed"}

// EntitlementStatus says whether a row can still grant access.
type EntitlementStatus string

// The entitlement statuses; migration 2 checks the column against them.
const (
	EntitlementActive EntitlementStatus = "active"
	EntitlementEnded  EntitlementStatus = "ended"
)

// Source is where an entitlement came from.
type Source string

// The entitlement sources; migration 2 checks the column against them.
const (
	SourceStripe Source = "stripe"
	SourceManual Source = "manual"
	SourceTrial  Source = "trial"
)

// Entitlement is one grant of access. An account's access is the union of its active rows.
type Entitlement struct {
	ID          int64
	AccountID   int64
	Source      Source
	Games       []string
	StoreScope  string
	Modes       []string
	Addons      []string
	Status      EntitlementStatus
	ValidFrom   time.Time
	ValidUntil  *time.Time
	ExternalRef string
	Note        string
	CreatedAt   time.Time
}

// ActiveAt reports whether the row grants access at now.
func (e Entitlement) ActiveAt(now time.Time) bool {
	if e.Status != EntitlementActive || now.Before(e.ValidFrom) {
		return false
	}
	return e.ValidUntil == nil || now.Before(*e.ValidUntil)
}

// IsActiveStripePlan reports whether e's Stripe subscription is still live,
// counting a past_due row even past its grace: Stripe still bills it.
func (e Entitlement) IsActiveStripePlan() bool {
	return e.Source == SourceStripe && e.Status == EntitlementActive
}

// HasActiveStripePlan reports whether ents includes a live Stripe subscription.
func HasActiveStripePlan(ents []Entitlement) bool {
	for _, e := range ents {
		if e.IsActiveStripePlan() {
			return true
		}
	}
	return false
}

// ValidateStoreScope canonicalizes a preset or a list of backend shorthands, checking syntax only.
func ValidateStoreScope(scope string) (string, error) {
	return canonicalStoreScope(scope)
}

// presetScopeNames are only valid standing alone; one inside a comma list is rejected.
var presetScopeNames = []string{ScopeAll, ScopeBase, "DEV_ACCESS"}

// canonicalStoreScope canonicalizes scope; operators type shorthands and nothing checks them against a list.
// Presets are case-insensitive and must stand alone; explicit tokens are backend shorthands and keep their case.
func canonicalStoreScope(scope string) (string, error) {
	scope = strings.TrimSpace(scope)
	switch strings.ToUpper(scope) {
	case ScopeAll, ScopeBase:
		return strings.ToUpper(scope), nil
	case "DEV_ACCESS":
		return "", errors.New("DEV_ACCESS cannot be granted")
	}
	var out []string
	for _, part := range strings.Split(scope, ",") {
		s := strings.TrimSpace(part)
		if s == "" {
			continue
		}
		if slices.ContainsFunc(presetScopeNames, func(p string) bool { return strings.EqualFold(p, s) }) {
			return "", fmt.Errorf("%q is a preset and must stand alone", s)
		}
		if strings.ContainsFunc(s, unicode.IsSpace) {
			return "", fmt.Errorf("store %q has a space; separate stores with commas", s)
		}
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return "", errors.New("store scope is empty")
	}
	slices.Sort(out)
	return strings.Join(out, ","), nil
}

// ValidateModes deduplicates and orders modes; "all" is not a mode.
func ValidateModes(modes []string) ([]string, error) {
	var out []string
	for _, valid := range ValidModes {
		for _, m := range modes {
			if strings.ToLower(strings.TrimSpace(m)) == valid && !slices.Contains(out, valid) {
				out = append(out, valid)
			}
		}
	}
	for _, m := range modes {
		if !slices.Contains(ValidModes, strings.ToLower(strings.TrimSpace(m))) {
			return nil, fmt.Errorf("unknown mode %q", m)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no modes given")
	}
	return out, nil
}

const entitlementCols = "id, account_id, source, games, store_scope, modes, addons, status, valid_from, valid_until, external_ref, note, created_at"

func scanEntitlement(row scanner) (Entitlement, error) {
	var e Entitlement
	var until sql.NullTime
	var ext sql.NullString
	err := row.Scan(&e.ID, &e.AccountID, &e.Source, pq.Array(&e.Games), &e.StoreScope, pq.Array(&e.Modes),
		pq.Array(&e.Addons), &e.Status, &e.ValidFrom, &until, &ext, &e.Note, &e.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Entitlement{}, ErrNotFound
	}
	if until.Valid {
		t := until.Time
		e.ValidUntil = &t
	}
	e.ExternalRef = ext.String
	if e.Addons == nil {
		e.Addons = []string{}
	}
	return e, err
}

// querier is satisfied by both *sql.DB and *sql.Tx.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// prepareEntitlement canonicalizes and validates e and returns the nullable columns.
func prepareEntitlement(e Entitlement) (Entitlement, sql.NullTime, sql.NullString, error) {
	scope, err := canonicalStoreScope(e.StoreScope)
	if err != nil {
		return Entitlement{}, sql.NullTime{}, sql.NullString{}, err
	}
	e.StoreScope = scope
	modes, err := ValidateModes(e.Modes)
	if err != nil {
		return Entitlement{}, sql.NullTime{}, sql.NullString{}, err
	}
	e.Modes = modes
	if e.Status == "" {
		e.Status = EntitlementActive
	}
	if e.ValidFrom.IsZero() {
		e.ValidFrom = time.Now()
	}
	if e.Addons == nil {
		e.Addons = []string{}
	}
	var until sql.NullTime
	if e.ValidUntil != nil {
		until = sql.NullTime{Time: *e.ValidUntil, Valid: true}
	}
	var ext sql.NullString
	if e.ExternalRef != "" {
		ext = sql.NullString{String: e.ExternalRef, Valid: true}
	}
	return e, until, ext, nil
}

// AddEntitlement canonicalizes e's scope and modes, validates their
// syntax (a preset is refused anywhere but alone), then inserts it.
func (c *Client) AddEntitlement(ctx context.Context, e Entitlement) (Entitlement, error) {
	e, until, ext, err := prepareEntitlement(e)
	if err != nil {
		return Entitlement{}, err
	}
	return insertEntitlement(ctx, c.db, e, until, ext)
}

// insertEntitlement inserts an already-prepared e against q, a *sql.DB or a
// *sql.Tx, so CreateTrial can add it inside its own transaction.
func insertEntitlement(ctx context.Context, q querier, e Entitlement, until sql.NullTime, ext sql.NullString) (Entitlement, error) {
	return scanEntitlement(q.QueryRowContext(ctx,
		`INSERT INTO entitlements (account_id, source, games, store_scope, modes, addons, status, valid_from, valid_until, external_ref, note)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING `+entitlementCols,
		e.AccountID, e.Source, pq.Array(e.Games), e.StoreScope, pq.Array(e.Modes), pq.Array(e.Addons),
		e.Status, e.ValidFrom, until, ext, e.Note))
}

// UpsertStripeEntitlement inserts or updates the one row for e.ExternalRef.
// valid_from is kept from the first insert; everything else follows e.
func (c *Client) UpsertStripeEntitlement(ctx context.Context, e Entitlement) (Entitlement, error) {
	if e.ExternalRef == "" {
		return Entitlement{}, errors.New("apiaccess: external_ref is required")
	}
	e, until, ext, err := prepareEntitlement(e)
	if err != nil {
		return Entitlement{}, err
	}
	return scanEntitlement(c.db.QueryRowContext(ctx,
		`INSERT INTO entitlements (account_id, source, games, store_scope, modes, addons, status, valid_from, valid_until, external_ref, note)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		 ON CONFLICT (external_ref) WHERE external_ref IS NOT NULL DO UPDATE SET
		   account_id = EXCLUDED.account_id, source = EXCLUDED.source, games = EXCLUDED.games,
		   store_scope = EXCLUDED.store_scope, modes = EXCLUDED.modes, addons = EXCLUDED.addons,
		   status = EXCLUDED.status, valid_until = EXCLUDED.valid_until, note = EXCLUDED.note
		 RETURNING `+entitlementCols,
		e.AccountID, e.Source, pq.Array(e.Games), e.StoreScope, pq.Array(e.Modes), pq.Array(e.Addons),
		e.Status, e.ValidFrom, until, ext, e.Note))
}

// StripeRef is one active stripe row: the account and its subscription id.
type StripeRef struct {
	AccountID int64
	SubID     string
}

// ListActiveStripeRefs returns every active stripe row, ordered by account and subscription id.
func (c *Client) ListActiveStripeRefs(ctx context.Context) ([]StripeRef, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT account_id, external_ref FROM entitlements WHERE source = 'stripe' AND status = 'active' AND external_ref IS NOT NULL ORDER BY account_id, external_ref`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []StripeRef
	for rows.Next() {
		var ref StripeRef
		if err := rows.Scan(&ref.AccountID, &ref.SubID); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// EndEntitlement marks the row ended as of at and returns it. accountID 0
// means any account; otherwise the row must belong to it, as with RevokeKey.
// A source = 'stripe' row is never updated: the next reconcile would just
// restore it from Stripe, so this refuses with ErrStripeEntitlement instead.
func (c *Client) EndEntitlement(ctx context.Context, id, accountID int64, at time.Time) (Entitlement, error) {
	e, err := scanEntitlement(c.db.QueryRowContext(ctx,
		`UPDATE entitlements SET status = 'ended', valid_until = $2
		  WHERE id = $1 AND status <> 'ended' AND source <> 'stripe' AND ($3 = 0 OR account_id = $3)
		  RETURNING `+entitlementCols, id, at, accountID))
	if err == nil {
		return e, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Entitlement{}, err
	}
	// No row matched the update; look at the row as-is to tell a stripe
	// refusal apart from a genuine not-found or already-ended row.
	row, rowErr := scanEntitlement(c.db.QueryRowContext(ctx,
		`SELECT `+entitlementCols+` FROM entitlements WHERE id = $1 AND ($2 = 0 OR account_id = $2)`, id, accountID))
	if rowErr != nil {
		return Entitlement{}, rowErr
	}
	if row.Source == SourceStripe {
		return Entitlement{}, ErrStripeEntitlement
	}
	return Entitlement{}, ErrNotFound
}

// ListEntitlements returns every row for the account, oldest first.
func (c *Client) ListEntitlements(ctx context.Context, accountID int64) ([]Entitlement, error) {
	return c.listEntitlements(ctx, accountID, false)
}

func (c *Client) listEntitlements(ctx context.Context, accountID int64, activeOnly bool) ([]Entitlement, error) {
	query := `SELECT ` + entitlementCols + ` FROM entitlements WHERE account_id = $1`
	if activeOnly {
		query += ` AND status = 'active'`
	}
	rows, err := c.db.QueryContext(ctx, query+` ORDER BY id`, accountID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Entitlement{}
	for rows.Next() {
		e, err := scanEntitlement(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
