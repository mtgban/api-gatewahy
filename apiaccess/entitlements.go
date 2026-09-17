package apiaccess

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/lib/pq"
)

// Store scope presets the backend understands. DEV_ACCESS is never issued.
const (
	ScopeAll  = "ALL_ACCESS"
	ScopeBase = "BASE_ACCESS"
)

// ValidModes in canonical order.
var ValidModes = []string{"retail", "buylist", "sealed"}

// Entitlement is one grant of access. An account's access is the union of its active rows.
type Entitlement struct {
	ID          int64
	AccountID   int64
	Source      string
	Games       []string
	StoreScope  string
	Modes       []string
	Addons      []string
	Status      string
	ValidFrom   time.Time
	ValidUntil  *time.Time
	ExternalRef string
	Note        string
	CreatedAt   time.Time
}

// ActiveAt reports whether the row grants access at now.
func (e Entitlement) ActiveAt(now time.Time) bool {
	if e.Status != "active" || now.Before(e.ValidFrom) {
		return false
	}
	return e.ValidUntil == nil || now.Before(*e.ValidUntil)
}

// ValidateStoreScope canonicalizes a preset or a store list against knownStores.
func ValidateStoreScope(scope string, knownStores []string) (string, error) {
	return canonicalStoreScope(scope, knownStores, true)
}

// canonicalStoreScope canonicalizes scope; checkKnown false accepts any store token.
func canonicalStoreScope(scope string, knownStores []string, checkKnown bool) (string, error) {
	scope = strings.TrimSpace(scope)
	switch strings.ToUpper(scope) {
	case ScopeAll, ScopeBase:
		return strings.ToUpper(scope), nil
	case "DEV_ACCESS":
		return "", errors.New("DEV_ACCESS cannot be granted")
	}
	known := map[string]bool{}
	for _, s := range knownStores {
		known[strings.ToUpper(s)] = true
	}
	var out []string
	for _, part := range strings.Split(scope, ",") {
		s := strings.ToUpper(strings.TrimSpace(part))
		if s == "" {
			continue
		}
		if checkKnown && !known[s] {
			return "", fmt.Errorf("unknown store %q", s)
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

// AddEntitlement canonicalizes and validates e, then inserts it. Every writer
// goes through here, so no caller can store a scope or mode the gateway rejects.
func (c *Client) AddEntitlement(ctx context.Context, e Entitlement) (Entitlement, error) {
	scope, err := canonicalStoreScope(e.StoreScope, c.KnownStores, len(c.KnownStores) > 0)
	if err != nil {
		return Entitlement{}, err
	}
	e.StoreScope = scope
	modes, err := ValidateModes(e.Modes)
	if err != nil {
		return Entitlement{}, err
	}
	e.Modes = modes
	if e.Status == "" {
		e.Status = "active"
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
	return scanEntitlement(c.db.QueryRowContext(ctx,
		`INSERT INTO entitlements (account_id, source, games, store_scope, modes, addons, status, valid_from, valid_until, external_ref, note)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING `+entitlementCols,
		e.AccountID, e.Source, pq.Array(e.Games), e.StoreScope, pq.Array(e.Modes), pq.Array(e.Addons),
		e.Status, e.ValidFrom, until, ext, e.Note))
}

// EndEntitlement marks the row ended as of at.
func (c *Client) EndEntitlement(ctx context.Context, id int64, at time.Time) error {
	res, err := c.db.ExecContext(ctx,
		`UPDATE entitlements SET status = 'ended', valid_until = $2 WHERE id = $1`, id, at)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
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
