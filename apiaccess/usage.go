package apiaccess

import (
	"context"
	"database/sql"
	"net/netip"
	"time"

	"github.com/lib/pq"
)

// Usage is one proxied request.
type Usage struct {
	Ts         time.Time
	KeyID      int64
	AccountID  int64
	Game       string
	Path       string
	Status     int
	Bytes      int64
	DurationMS int
	ClientIP   string
}

// UsageRow is one line of a summary: an account and game over a period.
type UsageRow struct {
	AccountID int64
	Email     string
	Game      string
	Requests  int64
	Bytes     int64
	Errors    int64
}

// InsertUsage writes rows in one COPY-style batch.
func (c *Client) InsertUsage(ctx context.Context, rows []Usage) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, pq.CopyIn("usage",
		"ts", "key_id", "account_id", "game", "path", "status", "bytes", "duration_ms", "client_ip"))
	if err != nil {
		return err
	}
	for _, u := range rows {
		var ip any
		// client_ip is inet, so an unparseable or zoned address would reject the batch.
		if addr, perr := netip.ParseAddr(u.ClientIP); perr == nil && addr.Zone() == "" {
			ip = addr.Unmap().String()
		}
		if _, err := stmt.ExecContext(ctx, u.Ts, u.KeyID, u.AccountID, u.Game, u.Path, u.Status, u.Bytes, u.DurationMS, ip); err != nil {
			return err
		}
	}
	if _, err := stmt.ExecContext(ctx); err != nil {
		return err
	}
	if err := stmt.Close(); err != nil {
		return err
	}
	return tx.Commit()
}

// SummarizeUsage groups requests by account and game within [since, until).
func (c *Client) SummarizeUsage(ctx context.Context, since, until time.Time, accountID int64) ([]UsageRow, error) {
	var filter sql.NullInt64
	if accountID != 0 {
		filter = sql.NullInt64{Int64: accountID, Valid: true}
	}
	rows, err := c.db.QueryContext(ctx,
		`SELECT u.account_id, a.email, u.game, count(*), coalesce(sum(u.bytes), 0),
		        count(*) FILTER (WHERE u.status >= 400)
		   FROM usage u JOIN accounts a ON a.id = u.account_id
		  WHERE u.ts >= $1 AND u.ts < $2 AND ($3::bigint IS NULL OR u.account_id = $3)
		  GROUP BY u.account_id, a.email, u.game
		  ORDER BY a.email, u.game`, since, until, filter)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []UsageRow
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(&r.AccountID, &r.Email, &r.Game, &r.Requests, &r.Bytes, &r.Errors); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PruneUsage deletes rows older than before and returns how many.
func (c *Client) PruneUsage(ctx context.Context, before time.Time) (int64, error) {
	res, err := c.db.ExecContext(ctx, `DELETE FROM usage WHERE ts < $1`, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
