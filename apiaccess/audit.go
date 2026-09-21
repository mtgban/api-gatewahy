package apiaccess

import (
	"context"
	"database/sql"
	"time"
)

// AdminAction is one operator action, kept for accountability.
type AdminAction struct {
	ID        int64
	At        time.Time
	Actor     string
	Action    string
	AccountID int64
	Target    string
	Detail    string
}

// RecordAdminAction writes one audit row. accountID 0 means no account.
func (c *Client) RecordAdminAction(ctx context.Context, actor, action string, accountID int64, target, detail string) error {
	var acct sql.NullInt64
	if accountID != 0 {
		acct = sql.NullInt64{Int64: accountID, Valid: true}
	}
	_, err := c.db.ExecContext(ctx,
		`INSERT INTO admin_actions (actor, action, account_id, target, detail) VALUES ($1, $2, $3, $4, $5)`,
		NormalizeEmail(actor), action, acct, target, detail)
	return err
}

// ListAdminActions returns the newest actions, for one account or for all when accountID is 0.
func (c *Client) ListAdminActions(ctx context.Context, accountID int64, limit int) ([]AdminAction, error) {
	var filter sql.NullInt64
	if accountID != 0 {
		filter = sql.NullInt64{Int64: accountID, Valid: true}
	}
	rows, err := c.db.QueryContext(ctx,
		`SELECT id, at, actor, action, coalesce(account_id, 0), target, detail FROM admin_actions
		  WHERE $1::bigint IS NULL OR account_id = $1 ORDER BY at DESC, id DESC LIMIT $2`, filter, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AdminAction
	for rows.Next() {
		var a AdminAction
		if err := rows.Scan(&a.ID, &a.At, &a.Actor, &a.Action, &a.AccountID, &a.Target, &a.Detail); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
