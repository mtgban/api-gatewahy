package apiaccess

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestGenerateKeyShape(t *testing.T) {
	for kind, want := range map[KeyKind]string{KeyLive: `^ban_live_[a-z0-9]{32}$`, KeyDemo: `^ban_demo_[a-z0-9]{32}$`} {
		plain, hash, prefix, err := GenerateKey(kind)
		if err != nil {
			t.Fatal(err)
		}
		if !regexp.MustCompile(want).MatchString(plain) {
			t.Errorf("%s key %q", kind, plain)
		}
		if hash != HashKey(plain) || len(prefix) != 8 || !strings.HasPrefix(plain, string(kind)+"_"+prefix) {
			t.Errorf("hash %q prefix %q for %q", hash, prefix, plain)
		}
		if !LooksLikeKey(plain) {
			t.Errorf("%q does not look like a key", plain)
		}
	}
	for _, bad := range []string{"", "ban_live_short", "ban_test_" + strings.Repeat("a", 32), "BAN_LIVE_" + strings.Repeat("a", 32)} {
		if LooksLikeKey(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
	// Keys minted before the rename keep working.
	if !LooksLikeKey("mtgban_live_" + strings.Repeat("a", 32)) {
		t.Error("legacy prefix refused")
	}
	if _, _, _, err := GenerateKey(KeyKind("bogus")); err == nil {
		t.Error("invalid kind accepted")
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

	plain, k, err := c.CreateKey(ctx, a.ID, "prod", KeyLive)
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

	recent, _ := c.KeysCreatedBetween(ctx, time.Now().Add(-time.Minute), time.Now().Add(time.Minute))
	if len(recent) != 1 {
		t.Errorf("recent keys %d", len(recent))
	}
}

func TestKeysCreatedBetweenHalfOpenWindow(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	a, _ := c.CreateAccount(ctx, "window@example.com", "")

	from := time.Now().UTC().Truncate(time.Second)
	to := from.Add(time.Hour)

	seed := func(at time.Time, label string) {
		_, hash, prefix, err := GenerateKey(KeyLive)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.db.ExecContext(ctx,
			`INSERT INTO api_keys (account_id, key_hash, prefix, label, kind, created_at) VALUES ($1, $2, $3, $4, $5, $6)`,
			a.ID, hash, prefix, label, string(KeyLive), at); err != nil {
			t.Fatal(err)
		}
	}
	seed(from.Add(-time.Second), "before-from")
	seed(from, "at-from")
	seed(to.Add(-time.Second), "before-to")
	seed(to, "at-to")

	got, err := c.KeysCreatedBetween(ctx, from, to)
	if err != nil {
		t.Fatal(err)
	}
	var labels []string
	for _, k := range got {
		labels = append(labels, k.Label)
	}
	want := []string{"at-from", "before-to"}
	if !slices.Equal(labels, want) {
		t.Errorf("labels %v, want %v", labels, want)
	}
}

// TestRevokeKeyByPrefixIsAtomic holds a row lock so RevokeKeyByPrefix reads
// the key as live, then blocks on a revoke that lands first.
func TestRevokeKeyByPrefixIsAtomic(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	a, _ := c.CreateAccount(ctx, "race-revoke@example.com", "")
	_, k, err := c.CreateKey(ctx, a.ID, "", KeyLive)
	if err != nil {
		t.Fatal(err)
	}

	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT id FROM api_keys WHERE id = $1 FOR UPDATE`, k.ID); err != nil {
		t.Fatal(err)
	}
	var lockerPID int
	if err := tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&lockerPID); err != nil {
		t.Fatal(err)
	}

	results := make(chan struct {
		key Key
		err error
	}, 1)
	go func() {
		key, err := c.RevokeKeyByPrefix(ctx, k.Prefix)
		results <- struct {
			key Key
			err error
		}{key, err}
	}()
	if !waitUntilBlockedOn(ctx, t, c, lockerPID, 5*time.Second) {
		t.Fatal("the revoke never blocked on the held row lock")
	}

	if _, err := tx.ExecContext(ctx, `UPDATE api_keys SET revoked_at = now() WHERE id = $1`, k.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	res := <-results
	if !errors.Is(res.err, ErrNotFound) {
		t.Errorf("stale caller's revoke: got %+v %v, want ErrNotFound", res.key, res.err)
	}
	got, err := c.ListKeys(ctx, a.ID)
	if err != nil || len(got) != 1 || got[0].RevokedAt == nil {
		t.Fatalf("key state after both calls: %+v %v", got, err)
	}
}

// waitUntilBlockedOn polls until some backend is waiting on lockerPID's lock,
// rather than sleeping a fixed guess at how long that takes.
func waitUntilBlockedOn(ctx context.Context, t *testing.T, c *Client, lockerPID int, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var blocked bool
		err := c.db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid)))`, lockerPID).Scan(&blocked)
		if err != nil {
			t.Fatal(err)
		}
		if blocked {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func TestCreateKeyRetriesTakenPrefix(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	a, err := c.CreateAccount(ctx, "dup@example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	taken, takenHash, takenPrefix, err := GenerateKey(KeyLive)
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
	generateKey = func(kind KeyKind) (string, string, string, error) {
		plain, hash, prefix, err := orig(kind)
		if !collided {
			collided = true
			return plain, hash, takenPrefix, err
		}
		return plain, hash, prefix, err
	}

	plain, k, err := c.CreateKey(ctx, a.ID, "second", KeyLive)
	if err != nil {
		t.Fatalf("create after collision: %v", err)
	}
	if !collided || k.Prefix == takenPrefix || plain == taken {
		t.Errorf("collided %v, created %+v", collided, k)
	}
}

func TestKeyKindRoundTrip(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	a, _ := c.CreateAccount(ctx, "kind@example.com", "")

	plain, live, err := c.CreateKey(ctx, a.ID, "live", KeyLive)
	if err != nil {
		t.Fatal(err)
	}
	if live.Kind != KeyLive {
		t.Errorf("created live key kind %q", live.Kind)
	}
	demoPlain, demo, err := c.CreateKey(ctx, a.ID, "demo", KeyDemo)
	if err != nil {
		t.Fatal(err)
	}
	if demo.Kind != KeyDemo {
		t.Errorf("created demo key kind %q", demo.Kind)
	}

	lk, err := c.LookupKey(ctx, HashKey(plain))
	if err != nil || lk.Key.Kind != KeyLive {
		t.Fatalf("live lookup %+v %v", lk.Key, err)
	}
	lk, err = c.LookupKey(ctx, HashKey(demoPlain))
	if err != nil || lk.Key.Kind != KeyDemo {
		t.Fatalf("demo lookup %+v %v", lk.Key, err)
	}

	list, err := c.ListKeys(ctx, a.ID)
	if err != nil || len(list) != 2 {
		t.Fatalf("list %d %v", len(list), err)
	}
	if list[0].Kind != KeyLive || list[1].Kind != KeyDemo {
		t.Errorf("listed kinds %q %q", list[0].Kind, list[1].Kind)
	}
}
