package apiaccess

import (
	"context"
	"net/netip"
	"time"
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
	stmt, err := tx.PrepareContext(ctx,
		`COPY usage (ts, key_id, account_id, game, path, status, bytes, duration_ms, client_ip) FROM STDIN`)
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

func scanUsageRow(row scanner) (UsageRow, error) {
	var r UsageRow
	err := row.Scan(&r.AccountID, &r.Email, &r.Game, &r.Requests, &r.Bytes, &r.Errors)
	return r, err
}

// SummarizeUsage groups requests by account and game within [since, until).
func (c *Client) SummarizeUsage(ctx context.Context, since, until time.Time, accountID int64) ([]UsageRow, error) {
	return queryAll(ctx, c.db, scanUsageRow,
		`SELECT u.account_id, a.email, u.game, count(*), coalesce(sum(u.bytes), 0),
		        count(*) FILTER (WHERE u.status >= 400)
		   FROM usage u JOIN accounts a ON a.id = u.account_id
		  WHERE u.ts >= $1 AND u.ts < $2 AND ($3::bigint IS NULL OR u.account_id = $3)
		  GROUP BY u.account_id, a.email, u.game
		  ORDER BY a.email, u.game`, since, until, optionalID(accountID))
}

// PruneUsage deletes rows older than before and returns how many.
func (c *Client) PruneUsage(ctx context.Context, before time.Time) (int64, error) {
	return c.deleteCount(ctx, `DELETE FROM usage WHERE ts < $1`, before)
}

// deleteCount runs one DELETE and returns how many rows it removed.
func (c *Client) deleteCount(ctx context.Context, query string, args ...any) (int64, error) {
	res, err := c.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// KeyUsageRow is one key on one UTC day.
type KeyUsageRow struct {
	KeyID    int64
	Prefix   string
	Label    string
	Day      time.Time
	Requests int64
	Bytes    int64
	Errors   int64
}

func scanKeyUsageRow(row scanner) (KeyUsageRow, error) {
	var r KeyUsageRow
	err := row.Scan(&r.KeyID, &r.Prefix, &r.Label, &r.Day, &r.Requests, &r.Bytes, &r.Errors)
	return r, err
}

// UsageByKey summarizes usage per key per day; accountID 0 means every account.
func (c *Client) UsageByKey(ctx context.Context, since, until time.Time, accountID int64) ([]KeyUsageRow, error) {
	return queryAll(ctx, c.db, scanKeyUsageRow,
		`SELECT u.key_id, k.prefix, k.label, date_trunc('day', u.ts AT TIME ZONE 'UTC') AS day,
		        count(*), coalesce(sum(u.bytes), 0), count(*) FILTER (WHERE u.status >= 400)
		   FROM usage u JOIN api_keys k ON k.id = u.key_id
		  WHERE u.ts >= $1 AND u.ts < $2 AND ($3::bigint IS NULL OR u.account_id = $3)
		  GROUP BY u.key_id, k.prefix, k.label, day
		  ORDER BY u.key_id, day`, since, until, optionalID(accountID))
}

// PathUsageRow is one path's request count for a key.
type PathUsageRow struct {
	Path     string
	Requests int64
	Errors   int64
}

func scanPathUsageRow(row scanner) (PathUsageRow, error) {
	var r PathUsageRow
	err := row.Scan(&r.Path, &r.Requests, &r.Errors)
	return r, err
}

// TopPaths lists the paths one key requested most, up to limit.
func (c *Client) TopPaths(ctx context.Context, since, until time.Time, keyID int64, limit int) ([]PathUsageRow, error) {
	if limit <= 0 {
		limit = 10
	}
	return queryAll(ctx, c.db, scanPathUsageRow,
		`SELECT u.path, count(*), count(*) FILTER (WHERE u.status >= 400)
		   FROM usage u
		  WHERE u.ts >= $1 AND u.ts < $2 AND u.key_id = $3
		  GROUP BY u.path
		  ORDER BY count(*) DESC, u.path
		  LIMIT $4`, since, until, keyID, limit)
}
