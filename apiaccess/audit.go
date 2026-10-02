package apiaccess

import (
	"context"
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
	_, err := c.db.ExecContext(ctx,
		`INSERT INTO admin_actions (actor, action, account_id, target, detail) VALUES ($1, $2, $3, $4, $5)`,
		NormalizeEmail(actor), action, optionalID(accountID), target, detail)
	return err
}

func scanAdminAction(row scanner) (AdminAction, error) {
	var a AdminAction
	err := row.Scan(&a.ID, &a.At, &a.Actor, &a.Action, &a.AccountID, &a.Target, &a.Detail)
	return a, err
}

// ListAdminActions returns the newest actions, for one account or for all when accountID is 0.
func (c *Client) ListAdminActions(ctx context.Context, accountID int64, limit int) ([]AdminAction, error) {
	return queryAll(ctx, c.db, scanAdminAction,
		`SELECT id, at, actor, action, coalesce(account_id, 0), target, detail FROM admin_actions
		  WHERE $1::bigint IS NULL OR account_id = $1 ORDER BY at DESC, id DESC LIMIT $2`,
		optionalID(accountID), limit)
}

// PruneAdminActions deletes actions taken before the cutoff and returns how many.
func (c *Client) PruneAdminActions(ctx context.Context, before time.Time) (int64, error) {
	return c.deleteCount(ctx, `DELETE FROM admin_actions WHERE at < $1`, before)
}
