package apiaccess

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/lib/pq"
)

// KeyPrefix starts every customer key.
const KeyPrefix = "mtgban_live_"

const keyAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

var keyPattern = regexp.MustCompile(`^mtgban_live_[a-z0-9]{32}$`)

// Key is one bearer credential. The plaintext is never stored.
type Key struct {
	ID         int64
	AccountID  int64
	Hash       string
	Prefix     string
	Label      string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// Lookup is everything the gateway needs to decide a request.
type Lookup struct {
	Key          Key
	Account      Account
	Entitlements []Entitlement // active status rows only; callers still check ActiveAt
}

// GenerateKey returns a new plaintext key with its hash and display prefix.
func GenerateKey() (plaintext, hash, prefix string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", "", err
	}
	var b strings.Builder
	b.WriteString(KeyPrefix)
	for _, x := range buf {
		// Modulo bias is under 2% per char; irrelevant at 32 chars.
		b.WriteByte(keyAlphabet[int(x)%len(keyAlphabet)])
	}
	plaintext = b.String()
	return plaintext, HashKey(plaintext), plaintext[len(KeyPrefix) : len(KeyPrefix)+8], nil
}

// HashKey is the hex sha256 stored in place of the plaintext.
func HashKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// LooksLikeKey reports whether s has the shape of a customer key.
func LooksLikeKey(s string) bool {
	return keyPattern.MatchString(s)
}

const keyCols = "id, account_id, key_hash, prefix, label, created_at, last_used_at, revoked_at"

func nullTimePtr(n sql.NullTime) *time.Time {
	if !n.Valid {
		return nil
	}
	t := n.Time
	return &t
}

func scanKey(row scanner) (Key, error) {
	var k Key
	var last, revoked sql.NullTime
	err := row.Scan(&k.ID, &k.AccountID, &k.Hash, &k.Prefix, &k.Label, &k.CreatedAt, &last, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return Key{}, ErrNotFound
	}
	k.LastUsedAt = nullTimePtr(last)
	k.RevokedAt = nullTimePtr(revoked)
	return k, err
}

// generateKey is a variable so tests can force a prefix collision.
var generateKey = GenerateKey

// createKeyAttempts is the first insert plus three regenerations.
const createKeyAttempts = 4

// CreateKey mints a key for the account and returns the plaintext once.
// A prefix the live-prefix index already holds is regenerated.
func (c *Client) CreateKey(ctx context.Context, accountID int64, label string) (string, Key, error) {
	var err error
	for attempt := 0; attempt < createKeyAttempts; attempt++ {
		var plaintext, hash, prefix string
		plaintext, hash, prefix, err = generateKey()
		if err != nil {
			return "", Key{}, err
		}
		var k Key
		k, err = scanKey(c.db.QueryRowContext(ctx,
			`INSERT INTO api_keys (account_id, key_hash, prefix, label) VALUES ($1, $2, $3, $4) RETURNING `+keyCols,
			accountID, hash, prefix, label))
		if err == nil {
			return plaintext, k, nil
		}
		if !isUniqueViolation(err) {
			return "", Key{}, err
		}
	}
	return "", Key{}, err
}

// isUniqueViolation reports whether err is the Postgres unique violation 23505.
func isUniqueViolation(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23505"
}

// LookupKey returns the key, its account, and the account's active entitlements.
func (c *Client) LookupKey(ctx context.Context, hash string) (Lookup, error) {
	var lk Lookup
	var last, revoked sql.NullTime
	err := c.db.QueryRowContext(ctx,
		`SELECT k.id, k.account_id, k.key_hash, k.prefix, k.label, k.created_at, k.last_used_at, k.revoked_at,
		        a.id, a.email, a.status, a.created_at, a.note
		   FROM api_keys k JOIN accounts a ON a.id = k.account_id
		  WHERE k.key_hash = $1`, hash).Scan(
		&lk.Key.ID, &lk.Key.AccountID, &lk.Key.Hash, &lk.Key.Prefix, &lk.Key.Label, &lk.Key.CreatedAt, &last, &revoked,
		&lk.Account.ID, &lk.Account.Email, &lk.Account.Status, &lk.Account.CreatedAt, &lk.Account.Note)
	if errors.Is(err, sql.ErrNoRows) {
		return Lookup{}, ErrNotFound
	}
	if err != nil {
		return Lookup{}, err
	}
	lk.Key.LastUsedAt = nullTimePtr(last)
	lk.Key.RevokedAt = nullTimePtr(revoked)
	lk.Entitlements, err = c.listEntitlements(ctx, lk.Account.ID, true)
	return lk, err
}

// RevokeKeyByPrefix revokes the one live key whose prefix matches.
func (c *Client) RevokeKeyByPrefix(ctx context.Context, prefix string) (Key, error) {
	matches, err := c.queryKeys(ctx,
		`SELECT `+keyCols+` FROM api_keys WHERE prefix = $1 AND revoked_at IS NULL`, prefix)
	if err != nil {
		return Key{}, err
	}
	switch len(matches) {
	case 0:
		return Key{}, ErrNotFound
	case 1:
	default:
		return Key{}, fmt.Errorf("apiaccess: prefix %q matches %d live keys", prefix, len(matches))
	}
	return scanKey(c.db.QueryRowContext(ctx,
		`UPDATE api_keys SET revoked_at = now() WHERE id = $1 RETURNING `+keyCols, matches[0].ID))
}

// ListKeys returns the account's keys, oldest first, revoked included.
func (c *Client) ListKeys(ctx context.Context, accountID int64) ([]Key, error) {
	return c.queryKeys(ctx, `SELECT `+keyCols+` FROM api_keys WHERE account_id = $1 ORDER BY id`, accountID)
}

// KeysCreatedSince returns keys created at or after since, for the daily summary.
func (c *Client) KeysCreatedSince(ctx context.Context, since time.Time) ([]Key, error) {
	return c.queryKeys(ctx, `SELECT `+keyCols+` FROM api_keys WHERE created_at >= $1 ORDER BY id`, since)
}

func (c *Client) queryKeys(ctx context.Context, query string, args ...any) ([]Key, error) {
	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Key
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// TouchKeys records last_used_at for each key id, keeping the later value.
func (c *Client) TouchKeys(ctx context.Context, seen map[int64]time.Time) error {
	if len(seen) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(seen))
	times := make([]time.Time, 0, len(seen))
	for id, t := range seen {
		ids = append(ids, id)
		times = append(times, t)
	}
	_, err := c.db.ExecContext(ctx,
		`UPDATE api_keys k SET last_used_at = GREATEST(k.last_used_at, s.t)
		   FROM unnest($1::bigint[], $2::timestamptz[]) AS s(id, t)
		  WHERE k.id = s.id`, pq.Array(ids), pq.Array(times))
	return err
}
