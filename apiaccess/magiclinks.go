package apiaccess

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"time"
)

// CreateMagicLink mints a one-time sign-in token for the account and stores its hash.
func (c *Client) CreateMagicLink(ctx context.Context, accountID int64, ttl time.Duration) (string, error) {
	// Expired links are swept on the way in, mirroring ConsumeNonce.
	if _, err := c.db.ExecContext(ctx, `DELETE FROM magic_links WHERE expires_at < $1`, time.Now()); err != nil {
		return "", err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	_, err := c.db.ExecContext(ctx,
		`INSERT INTO magic_links (token_hash, account_id, expires_at) VALUES ($1, $2, $3)`,
		HashKey(token), accountID, time.Now().Add(ttl))
	if err != nil {
		return "", err
	}
	return token, nil
}

// ConsumeMagicLink marks a live token used and returns its account, or ErrNotFound.
func (c *Client) ConsumeMagicLink(ctx context.Context, token string, now time.Time) (Account, error) {
	return scanAccount(c.db.QueryRowContext(ctx,
		`WITH used AS (
		    UPDATE magic_links SET used_at = $2
		     WHERE token_hash = $1 AND used_at IS NULL AND expires_at > $2
		     RETURNING account_id)
		 SELECT `+accountCols+` FROM accounts WHERE id = (SELECT account_id FROM used)`,
		HashKey(token), now))
}

// DeleteMagicLink removes a token whose mail never went out.
func (c *Client) DeleteMagicLink(ctx context.Context, token string) error {
	_, err := c.db.ExecContext(ctx, `DELETE FROM magic_links WHERE token_hash = $1`, HashKey(token))
	return err
}
