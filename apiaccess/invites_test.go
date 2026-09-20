package apiaccess

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestInviteRoundTrip(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	now := time.Now()

	token, inv, err := c.CreateInvite(ctx, "quarterly", "CK@Example.com", 14*24*time.Hour, "card kingdom")
	if err != nil {
		t.Fatal(err)
	}
	if len(token) < 40 || inv.TokenHash != HashKey(token) || inv.IntervalKey != "quarterly" || inv.Email != "ck@example.com" || inv.UsedAt != nil {
		t.Errorf("created %q %+v", token, inv)
	}
	if !inv.ExpiresAt.After(now.Add(13 * 24 * time.Hour)) {
		t.Errorf("expires too soon: %v", inv.ExpiresAt)
	}

	if _, err := c.ConsumeInvite(ctx, token, "someone@else.com", now); !errors.Is(err, ErrInviteInvalid) {
		t.Errorf("wrong email accepted: %v", err)
	}
	used, err := c.ConsumeInvite(ctx, token, "ck@example.com", now)
	if err != nil || used.UsedAt == nil {
		t.Fatalf("consume: %+v %v", used, err)
	}
	if _, err := c.ConsumeInvite(ctx, token, "ck@example.com", now); !errors.Is(err, ErrInviteInvalid) {
		t.Errorf("second use accepted: %v", err)
	}
	if err := c.ReleaseInvite(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ConsumeInvite(ctx, token, "ck@example.com", now); err != nil {
		t.Errorf("released invite refused: %v", err)
	}
}

func TestInviteUnboundExpiredUnknown(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	now := time.Now()

	token, _, err := c.CreateInvite(ctx, "quarterly", "", time.Hour, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ConsumeInvite(ctx, token, "anyone@example.com", now); err != nil {
		t.Errorf("unbound invite refused: %v", err)
	}

	expired, _, err := c.CreateInvite(ctx, "quarterly", "", time.Hour, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ConsumeInvite(ctx, expired, "x@example.com", now.Add(2*time.Hour)); !errors.Is(err, ErrInviteInvalid) {
		t.Errorf("expired invite accepted: %v", err)
	}
	if _, err := c.ConsumeInvite(ctx, "not-a-token", "x@example.com", now); !errors.Is(err, ErrInviteInvalid) {
		t.Errorf("unknown token: %v", err)
	}
	if err := c.ReleaseInvite(ctx, "not-a-token"); err != nil {
		t.Errorf("release of unknown token errored: %v", err)
	}
}
