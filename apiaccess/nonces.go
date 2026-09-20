package apiaccess

import (
	"context"
	"errors"
	"time"
)

// ErrNonceUsed means the handoff token was already accepted once.
var ErrNonceUsed = errors.New("apiaccess: handoff nonce already used")

// ConsumeNonce records nonce until expiresAt; a second call inside that window fails.
// Expired rows are swept on the way in so the table stays small.
func (c *Client) ConsumeNonce(ctx context.Context, nonce string, expiresAt, now time.Time) error {
	if _, err := c.db.ExecContext(ctx, `DELETE FROM handoff_nonces WHERE expires_at < $1`, now); err != nil {
		return err
	}
	res, err := c.db.ExecContext(ctx,
		`INSERT INTO handoff_nonces (nonce, expires_at) VALUES ($1, $2) ON CONFLICT (nonce) DO NOTHING`, nonce, expiresAt)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNonceUsed
	}
	return nil
}
