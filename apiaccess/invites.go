package apiaccess

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"time"
)

// Invite authorizes one checkout on a non-public interval.
type Invite struct {
	TokenHash   string
	IntervalKey string
	Email       string
	ExpiresAt   time.Time
	UsedAt      *time.Time
	CreatedAt   time.Time
	Note        string
}

// ErrInviteInvalid covers unknown, used, expired, and wrong-email invites alike.
var ErrInviteInvalid = errors.New("apiaccess: invite is invalid, used, or expired")

const inviteCols = "token_hash, interval_key, email, expires_at, used_at, created_at, note"

func scanInvite(row scanner) (Invite, error) {
	var inv Invite
	var used sql.NullTime
	err := row.Scan(&inv.TokenHash, &inv.IntervalKey, &inv.Email, &inv.ExpiresAt, &used, &inv.CreatedAt, &inv.Note)
	if errors.Is(err, sql.ErrNoRows) {
		return Invite{}, ErrNotFound
	}
	inv.UsedAt = nullTimePtr(used)
	return inv, err
}

// CreateInvite mints a token, stores its hash, and returns the token once.
// An empty email leaves the invite usable by any account.
func (c *Client) CreateInvite(ctx context.Context, intervalKey, email string, ttl time.Duration, note string) (string, Invite, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", Invite{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	inv, err := scanInvite(c.db.QueryRowContext(ctx,
		`INSERT INTO invites (token_hash, interval_key, email, expires_at, note) VALUES ($1, $2, $3, $4, $5) RETURNING `+inviteCols,
		HashKey(token), intervalKey, NormalizeEmail(email), time.Now().Add(ttl), note))
	if err != nil {
		return "", Invite{}, err
	}
	return token, inv, nil
}

// ConsumeInvite marks the invite used if it is live and bound to email or to no one.
func (c *Client) ConsumeInvite(ctx context.Context, token, email string, now time.Time) (Invite, error) {
	inv, err := scanInvite(c.db.QueryRowContext(ctx,
		`UPDATE invites SET used_at = $2
		  WHERE token_hash = $1 AND used_at IS NULL AND expires_at > $2 AND (email = '' OR email = $3)
		  RETURNING `+inviteCols, HashKey(token), now, NormalizeEmail(email)))
	if errors.Is(err, ErrNotFound) {
		return Invite{}, ErrInviteInvalid
	}
	return inv, err
}

// ReleaseInvite clears used_at so a checkout that failed to start can retry.
func (c *Client) ReleaseInvite(ctx context.Context, token string) error {
	_, err := c.db.ExecContext(ctx, `UPDATE invites SET used_at = NULL WHERE token_hash = $1`, HashKey(token))
	return err
}
