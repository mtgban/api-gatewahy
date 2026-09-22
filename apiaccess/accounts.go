package apiaccess

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Account is one customer.
type Account struct {
	ID               int64
	Email            string
	Status           string
	CreatedAt        time.Time
	Note             string
	StripeCustomerID string
	// SessionEpoch is stamped into portal sessions; a logout bumps it.
	SessionEpoch int64
}

// NormalizeEmail lowercases and trims, which is how emails are stored and looked up.
func NormalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

const accountCols = "id, email, status, created_at, note, coalesce(stripe_customer_id, ''), session_epoch"

type scanner interface{ Scan(dest ...any) error }

func scanAccount(row scanner) (Account, error) {
	var a Account
	err := row.Scan(&a.ID, &a.Email, &a.Status, &a.CreatedAt, &a.Note, &a.StripeCustomerID, &a.SessionEpoch)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	return a, err
}

// CreateAccount inserts an active account. A duplicate email is an error.
func (c *Client) CreateAccount(ctx context.Context, email, note string) (Account, error) {
	return scanAccount(c.db.QueryRowContext(ctx,
		`INSERT INTO accounts (email, note) VALUES ($1, $2) RETURNING `+accountCols,
		NormalizeEmail(email), note))
}

// GetAccountByEmail returns the account with that email, or ErrNotFound.
func (c *Client) GetAccountByEmail(ctx context.Context, email string) (Account, error) {
	return scanAccount(c.db.QueryRowContext(ctx,
		`SELECT `+accountCols+` FROM accounts WHERE email = $1`, NormalizeEmail(email)))
}

// GetAccount returns the account with that id, or ErrNotFound.
func (c *Client) GetAccount(ctx context.Context, id int64) (Account, error) {
	return scanAccount(c.db.QueryRowContext(ctx,
		`SELECT `+accountCols+` FROM accounts WHERE id = $1`, id))
}

// SetAccountStatus sets "active" or "suspended".
func (c *Client) SetAccountStatus(ctx context.Context, id int64, status string) error {
	if status != "active" && status != "suspended" {
		return errors.New("apiaccess: status must be active or suspended")
	}
	res, err := c.db.ExecContext(ctx, `UPDATE accounts SET status = $2 WHERE id = $1`, id, status)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetStripeCustomerID records the customer once; a later call returns the first id.
func (c *Client) SetStripeCustomerID(ctx context.Context, accountID int64, customerID string) (string, error) {
	var got string
	err := c.db.QueryRowContext(ctx,
		`UPDATE accounts SET stripe_customer_id = coalesce(stripe_customer_id, $2) WHERE id = $1 RETURNING stripe_customer_id`,
		accountID, customerID).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return got, err
}

// GetAccountByStripeCustomer returns the account that owns the customer id.
func (c *Client) GetAccountByStripeCustomer(ctx context.Context, customerID string) (Account, error) {
	return scanAccount(c.db.QueryRowContext(ctx,
		`SELECT `+accountCols+` FROM accounts WHERE stripe_customer_id = $1`, customerID))
}

// GetOrCreateAccount returns the account for email, creating an active one
// with note when none exists. An existing row keeps its own note.
func (c *Client) GetOrCreateAccount(ctx context.Context, email, note string) (Account, error) {
	return scanAccount(c.db.QueryRowContext(ctx,
		`INSERT INTO accounts (email, note) VALUES ($1, $2)
		 ON CONFLICT (email) DO UPDATE SET email = EXCLUDED.email
		 RETURNING `+accountCols, NormalizeEmail(email), note))
}

// SetAccountNote replaces the operator note.
func (c *Client) SetAccountNote(ctx context.Context, id int64, note string) error {
	res, err := c.db.ExecContext(ctx, `UPDATE accounts SET note = $2 WHERE id = $1`, id, note)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SearchAccounts lists accounts whose email contains q, case-insensitively.
func (c *Client) SearchAccounts(ctx context.Context, q string) ([]Account, error) {
	pattern := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(strings.TrimSpace(q)) + "%"
	rows, err := c.db.QueryContext(ctx, `SELECT `+accountCols+` FROM accounts WHERE email ILIKE $1 ORDER BY email LIMIT 200`, pattern)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListAccounts returns every account, oldest first.
func (c *Client) ListAccounts(ctx context.Context) ([]Account, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT `+accountCols+` FROM accounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// BumpSessionEpoch signs out every portal session of the account and returns the new epoch.
func (c *Client) BumpSessionEpoch(ctx context.Context, accountID int64) (int64, error) {
	var epoch int64
	err := c.db.QueryRowContext(ctx, `UPDATE accounts SET session_epoch = session_epoch + 1 WHERE id = $1 RETURNING session_epoch`, accountID).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return epoch, err
}
