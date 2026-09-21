package apiaccess

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Trial is one Patreon trial grant.
type Trial struct {
	ID             int64
	PatreonEmail   string
	AccountID      int64
	GrantedAt      time.Time
	EndsAt         time.Time
	ReminderSentAt *time.Time
}

// ErrTrialTooSoon means the email had a trial inside the cooldown.
var ErrTrialTooSoon = errors.New("apiaccess: a trial was granted recently")

const trialCols = "id, patreon_email, account_id, granted_at, ends_at, reminder_sent_at"

func scanTrial(row scanner) (Trial, error) {
	var t Trial
	var reminded sql.NullTime
	err := row.Scan(&t.ID, &t.PatreonEmail, &t.AccountID, &t.GrantedAt, &t.EndsAt, &reminded)
	if errors.Is(err, sql.ErrNoRows) {
		return Trial{}, ErrNotFound
	}
	t.ReminderSentAt = nullTimePtr(reminded)
	return t, err
}

// CreateTrial records a trial unless one for email was granted after notBefore.
// A per-email advisory lock serializes concurrent grants.
func (c *Client) CreateTrial(ctx context.Context, email string, accountID int64, endsAt, notBefore time.Time) (Trial, error) {
	email = NormalizeEmail(email)
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return Trial{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "trial:"+email); err != nil {
		return Trial{}, err
	}
	t, err := scanTrial(tx.QueryRowContext(ctx,
		`INSERT INTO trials (patreon_email, account_id, ends_at)
		 SELECT $1, $2, $3
		  WHERE NOT EXISTS (SELECT 1 FROM trials WHERE patreon_email = $1 AND granted_at > $4)
		 RETURNING `+trialCols, email, accountID, endsAt, notBefore))
	if errors.Is(err, ErrNotFound) {
		return Trial{}, ErrTrialTooSoon
	}
	if err != nil {
		return Trial{}, err
	}
	return t, tx.Commit()
}

// LastTrial is the most recent trial for email, or ErrNotFound.
func (c *Client) LastTrial(ctx context.Context, email string) (Trial, error) {
	return scanTrial(c.db.QueryRowContext(ctx,
		`SELECT `+trialCols+` FROM trials WHERE patreon_email = $1 ORDER BY granted_at DESC LIMIT 1`, NormalizeEmail(email)))
}

// TrialsToRemind lists trials ending in [from, to) that were not reminded yet.
func (c *Client) TrialsToRemind(ctx context.Context, from, to time.Time) ([]Trial, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT `+trialCols+` FROM trials WHERE ends_at >= $1 AND ends_at < $2 AND reminder_sent_at IS NULL ORDER BY ends_at`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Trial
	for rows.Next() {
		t, err := scanTrial(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// MarkTrialReminded records that the ending-soon mail went out.
func (c *Client) MarkTrialReminded(ctx context.Context, id int64, at time.Time) error {
	res, err := c.db.ExecContext(ctx, `UPDATE trials SET reminder_sent_at = $2 WHERE id = $1`, id, at)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteTrial removes a trial whose entitlement could not be written.
func (c *Client) DeleteTrial(ctx context.Context, id int64) error {
	_, err := c.db.ExecContext(ctx, `DELETE FROM trials WHERE id = $1`, id)
	return err
}
