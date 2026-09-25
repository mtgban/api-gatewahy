package apiaccess

import (
	"context"
	"database/sql"
	"time"
)

// DemoAccess is one active trial or manual entitlement with what the admin
// wants beside it: who asked, how many keys the account minted, and when a
// key was last used.
type DemoAccess struct {
	AccountID int64
	Email     string
	Source    string
	Requester string
	Note      string
	GrantedAt time.Time
	EndsAt    *time.Time
	Keys      int
	LastUsed  *time.Time
}

// ListDemoAccess returns active trial and manual entitlements, newest first.
func (c *Client) ListDemoAccess(ctx context.Context) ([]DemoAccess, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT e.account_id, a.email, e.source,
		       CASE WHEN e.source = 'trial'
		            THEN coalesce((SELECT t.patreon_email FROM trials t WHERE t.account_id = e.account_id ORDER BY t.granted_at DESC LIMIT 1), '')
		            ELSE '' END,
		       e.note, e.valid_from, e.valid_until,
		       (SELECT count(*) FROM api_keys k WHERE k.account_id = e.account_id AND k.revoked_at IS NULL),
		       (SELECT max(k.last_used_at) FROM api_keys k WHERE k.account_id = e.account_id AND k.revoked_at IS NULL)
		  FROM entitlements e JOIN accounts a ON a.id = e.account_id
		 WHERE e.status = 'active' AND e.source IN ('trial', 'manual')
		   AND (e.valid_until IS NULL OR e.valid_until > now())
		 ORDER BY e.valid_from DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []DemoAccess
	for rows.Next() {
		var d DemoAccess
		var ends, last sql.NullTime
		if err := rows.Scan(&d.AccountID, &d.Email, &d.Source, &d.Requester, &d.Note, &d.GrantedAt, &ends, &d.Keys, &last); err != nil {
			return nil, err
		}
		if ends.Valid {
			t := ends.Time
			d.EndsAt = &t
		}
		if last.Valid {
			t := last.Time
			d.LastUsed = &t
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
