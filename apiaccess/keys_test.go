package apiaccess

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestGenerateKeyShape(t *testing.T) {
	plain, hash, prefix, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^mtgban_live_[a-z0-9]{32}$`).MatchString(plain) {
		t.Errorf("plaintext %q", plain)
	}
	if hash != HashKey(plain) || len(hash) != 64 {
		t.Errorf("hash %q", hash)
	}
	if prefix != strings.TrimPrefix(plain, KeyPrefix)[:8] {
		t.Errorf("prefix %q", prefix)
	}
	if !LooksLikeKey(plain) || LooksLikeKey("mtgban_live_short") || LooksLikeKey("") {
		t.Error("LooksLikeKey wrong")
	}
}

func TestKeysRoundTrip(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	a, _ := c.CreateAccount(ctx, "k@example.com", "")
	if _, err := c.AddEntitlement(ctx, Entitlement{
		AccountID: a.ID, Source: "manual", Games: []string{"magic"},
		StoreScope: "ALL_ACCESS", Modes: []string{"retail"},
	}); err != nil {
		t.Fatal(err)
	}

	plain, k, err := c.CreateKey(ctx, a.ID, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if k.Label != "prod" || k.RevokedAt != nil || k.AccountID != a.ID {
		t.Errorf("created %+v", k)
	}

	lk, err := c.LookupKey(ctx, HashKey(plain))
	if err != nil {
		t.Fatal(err)
	}
	if lk.Key.ID != k.ID || lk.Account.ID != a.ID || len(lk.Entitlements) != 1 {
		t.Errorf("lookup %+v", lk)
	}

	if _, err := c.LookupKey(ctx, HashKey("mtgban_live_nope")); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	if err := c.TouchKeys(ctx, map[int64]time.Time{k.ID: now}); err != nil {
		t.Fatal(err)
	}
	list, _ := c.ListKeys(ctx, a.ID)
	if len(list) != 1 || list[0].LastUsedAt == nil || !list[0].LastUsedAt.Equal(now) {
		t.Errorf("touch not recorded: %+v", list)
	}

	rk, err := c.RevokeKeyByPrefix(ctx, k.Prefix)
	if err != nil || rk.ID != k.ID {
		t.Fatalf("revoke: %+v %v", rk, err)
	}
	lk, _ = c.LookupKey(ctx, HashKey(plain))
	if lk.Key.RevokedAt == nil {
		t.Error("revoked key still reads as live")
	}
	if _, err := c.RevokeKeyByPrefix(ctx, k.Prefix); !errors.Is(err, ErrNotFound) {
		t.Errorf("second revoke: %v", err)
	}

	recent, _ := c.KeysCreatedSince(ctx, time.Now().Add(-time.Minute))
	if len(recent) != 1 {
		t.Errorf("recent keys %d", len(recent))
	}
}

func TestCreateKeyRetriesTakenPrefix(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	a, err := c.CreateAccount(ctx, "dup@example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	taken, takenHash, takenPrefix, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.ExecContext(ctx,
		`INSERT INTO api_keys (account_id, key_hash, prefix, label) VALUES ($1, $2, $3, 'seed')`,
		a.ID, takenHash, takenPrefix); err != nil {
		t.Fatal(err)
	}

	orig := generateKey
	t.Cleanup(func() { generateKey = orig })
	collided := false
	generateKey = func() (string, string, string, error) {
		plain, hash, prefix, err := orig()
		if !collided {
			collided = true
			return plain, hash, takenPrefix, err
		}
		return plain, hash, prefix, err
	}

	plain, k, err := c.CreateKey(ctx, a.ID, "second")
	if err != nil {
		t.Fatalf("create after collision: %v", err)
	}
	if !collided || k.Prefix == takenPrefix || plain == taken {
		t.Errorf("collided %v, created %+v", collided, k)
	}
}
