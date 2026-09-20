package apiaccess

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGetOrCreateAccountIsIdempotent(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	a, err := c.GetOrCreateAccount(ctx, " Ann@Example.com ", "first")
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.GetOrCreateAccount(ctx, "ann@example.com", "second")
	if err != nil || b.ID != a.ID || b.Note != "first" {
		t.Fatalf("second call: %+v %v", b, err)
	}
	if err := c.SetAccountNote(ctx, a.ID, "vip"); err != nil {
		t.Fatal(err)
	}
	got, err := c.SearchAccounts(ctx, "EXAMPLE")
	if err != nil || len(got) != 1 || got[0].Note != "vip" {
		t.Fatalf("search: %+v %v", got, err)
	}
	if got, _ := c.SearchAccounts(ctx, "nobody"); len(got) != 0 {
		t.Errorf("search miss returned %+v", got)
	}
}

func TestMagicLinkRoundTrip(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	a, _ := c.CreateAccount(ctx, "link@example.com", "")
	token, err := c.CreateMagicLink(ctx, a.ID, 15*time.Minute)
	if err != nil || len(token) < 32 {
		t.Fatalf("token %q %v", token, err)
	}
	now := time.Now()
	got, err := c.ConsumeMagicLink(ctx, token, now)
	if err != nil || got.ID != a.ID {
		t.Fatalf("consume: %+v %v", got, err)
	}
	if _, err := c.ConsumeMagicLink(ctx, token, now); !errors.Is(err, ErrNotFound) {
		t.Errorf("second use: %v", err)
	}
	expired, _ := c.CreateMagicLink(ctx, a.ID, 15*time.Minute)
	if _, err := c.ConsumeMagicLink(ctx, expired, now.Add(16*time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired: %v", err)
	}
	deleted, _ := c.CreateMagicLink(ctx, a.ID, 15*time.Minute)
	if err := c.DeleteMagicLink(ctx, deleted); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ConsumeMagicLink(ctx, deleted, now); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted: %v", err)
	}
	if _, err := c.ConsumeMagicLink(ctx, "nope", now); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}

	sweepAcct, _ := c.CreateAccount(ctx, "sweep@example.com", "")
	if _, err := c.CreateMagicLink(ctx, sweepAcct.ID, -time.Minute); err != nil {
		t.Fatal(err)
	}
	countLinks := func() int {
		var n int
		if err := c.db.QueryRowContext(ctx, `SELECT count(*) FROM magic_links WHERE account_id = $1`, sweepAcct.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := countLinks(); n != 1 {
		t.Fatalf("expired link count before sweep: %d", n)
	}
	if _, err := c.CreateMagicLink(ctx, sweepAcct.ID, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	if n := countLinks(); n != 1 {
		t.Errorf("expired link not swept: %d", n)
	}
}

func TestRevokeKeyIsScopedToAccount(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	a, _ := c.CreateAccount(ctx, "a@example.com", "")
	b, _ := c.CreateAccount(ctx, "b@example.com", "")
	_, k, err := c.CreateKey(ctx, a.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RevokeKey(ctx, k.ID, b.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("other account revoked the key: %v", err)
	}
	got, err := c.RevokeKey(ctx, k.ID, a.ID)
	if err != nil || got.RevokedAt == nil {
		t.Fatalf("revoke: %+v %v", got, err)
	}
	if _, err := c.RevokeKey(ctx, k.ID, 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("second revoke: %v", err)
	}
}

func TestTrialOncePerCooldown(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	a, _ := c.CreateAccount(ctx, "trial@example.com", "")
	now := time.Now().Truncate(time.Second)
	tr, err := c.CreateTrial(ctx, "Trial@Example.com", a.ID, now.AddDate(0, 0, 15), now.AddDate(0, 0, -180))
	if err != nil || tr.PatreonEmail != "trial@example.com" {
		t.Fatalf("first: %+v %v", tr, err)
	}
	if _, err := c.CreateTrial(ctx, "trial@example.com", a.ID, now.AddDate(0, 0, 15), now.AddDate(0, 0, -180)); !errors.Is(err, ErrTrialTooSoon) {
		t.Errorf("second: %v", err)
	}
	if _, err := c.CreateTrial(ctx, "trial@example.com", a.ID, now.AddDate(0, 0, 15), now.Add(time.Minute)); err != nil {
		t.Errorf("after cooldown: %v", err)
	}
	last, err := c.LastTrial(ctx, "trial@example.com")
	if err != nil || last.ID <= tr.ID {
		t.Errorf("last: %+v %v", last, err)
	}
	if _, err := c.LastTrial(ctx, "none@example.com"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	due, err := c.TrialsToRemind(ctx, now.AddDate(0, 0, 14), now.AddDate(0, 0, 16))
	if err != nil || len(due) != 2 {
		t.Fatalf("due: %+v %v", due, err)
	}
	if err := c.MarkTrialReminded(ctx, due[0].ID, now); err != nil {
		t.Fatal(err)
	}
	if due, _ = c.TrialsToRemind(ctx, now.AddDate(0, 0, 14), now.AddDate(0, 0, 16)); len(due) != 1 {
		t.Errorf("after mark: %d due", len(due))
	}
}

func TestDeleteTrialAllowsImmediateRetry(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	a, _ := c.CreateAccount(ctx, "retry@example.com", "")
	now := time.Now().Truncate(time.Second)
	tr, err := c.CreateTrial(ctx, "retry@example.com", a.ID, now.AddDate(0, 0, 15), now.AddDate(0, 0, -180))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := c.DeleteTrial(ctx, tr.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := c.CreateTrial(ctx, "retry@example.com", a.ID, now.AddDate(0, 0, 15), now.AddDate(0, 0, -180)); err != nil {
		t.Errorf("retry after delete: %v", err)
	}
}

func TestConsumeNonceIsSingleUse(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	now := time.Now()
	expiresAt := now.Add(5 * time.Minute)
	if err := c.ConsumeNonce(ctx, "abc", expiresAt, now); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := c.ConsumeNonce(ctx, "abc", expiresAt, now); !errors.Is(err, ErrNonceUsed) {
		t.Errorf("second: %v", err)
	}
	if err := c.ConsumeNonce(ctx, "abc", expiresAt, now.Add(6*time.Minute)); err != nil {
		t.Errorf("after expiry: %v", err)
	}
}
