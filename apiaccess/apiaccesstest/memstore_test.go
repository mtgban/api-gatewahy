package apiaccesstest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
)

// TestEveryStampReadsTheClock pins that no write reads the wall clock.
func TestEveryStampReadsTheClock(t *testing.T) {
	ctx := context.Background()
	frozen := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	m := New()
	m.Now = func() time.Time { return frozen }

	a, _ := m.CreateAccount(ctx, "clock@example.com", "")
	_, k, _ := m.CreateKey(ctx, a.ID, "", apiaccess.KeyLive)
	revoked, _ := m.RevokeKey(ctx, k.ID, 0)
	until := frozen.Add(time.Hour)
	e, _ := m.AddEntitlement(ctx, apiaccess.Entitlement{AccountID: a.ID, Source: "manual", Games: []string{"magic"}, StoreScope: "ALL_ACCESS", Modes: []string{"retail"}, ValidUntil: &until})
	tr, _ := m.CreateTrial(ctx, "clock@example.com", frozen.Add(time.Hour), frozen.Add(-time.Hour),
		apiaccess.Entitlement{AccountID: a.ID, Source: "trial", Games: []string{"magic"}, StoreScope: "ALL_ACCESS", Modes: []string{"retail"}})
	_, inv, _ := m.CreateInvite(ctx, "annual", "", time.Hour, "")
	_ = m.RecordAdminAction(ctx, "admin@example.com", "x", 0, "", "")
	acts, _ := m.ListAdminActions(ctx, 0, 1)
	for name, got := range map[string]time.Time{
		"account created": a.CreatedAt, "key created": k.CreatedAt, "key revoked": *revoked.RevokedAt,
		"valid_from": e.ValidFrom, "entitlement created": e.CreatedAt, "trial granted": tr.GrantedAt,
		"invite created": inv.CreatedAt, "invite expires": inv.ExpiresAt.Add(-time.Hour), "admin action": acts[0].At,
	} {
		if !got.Equal(frozen) {
			t.Errorf("%s: %v, want the store clock %v", name, got, frozen)
		}
	}

	token, _ := m.CreateMagicLink(ctx, a.ID, time.Minute)
	if _, err := m.ConsumeMagicLink(ctx, token, frozen.Add(time.Minute)); !errors.Is(err, apiaccess.ErrNotFound) {
		t.Errorf("link outlived its ttl on the store clock: %v", err)
	}
	if ok, _ := m.BeginStripeEvent(ctx, "evt", "x"); !ok {
		t.Fatal("first claim refused")
	}
	frozen = frozen.Add(6 * time.Minute)
	if ok, _ := m.BeginStripeEvent(ctx, "evt", "x"); !ok {
		t.Error("a stale claim was not reclaimed on the store clock")
	}
	if demo, _ := m.ListDemoAccess(ctx); len(demo) != 2 || !demo[1].GrantedAt.Equal(frozen.Add(-6*time.Minute)) {
		t.Errorf("demo access %+v", demo)
	}
}

func TestFailHooks(t *testing.T) {
	ctx := context.Background()
	m := New()
	down := errors.New("db down")
	m.Fail["GetAccount"] = down
	if _, err := m.GetAccount(ctx, 1); !errors.Is(err, down) {
		t.Errorf("fail: %v", err)
	}
	m.Fail["Notify"], m.FailAt["Notify"] = down, 2
	for i, want := range []error{nil, down, nil} {
		if err := m.Notify(ctx, "p"); !errors.Is(err, want) {
			t.Errorf("notify call %d: %v, want %v", i+1, err, want)
		}
	}
	if m.Calls["Notify"] != 3 || m.Calls["GetAccount"] != 1 || len(m.Notified) != 2 {
		t.Errorf("calls %v notified %v", m.Calls, m.Notified)
	}
	m.Fail["ListKeys"], m.FailFrom["ListKeys"] = down, 2
	for i, want := range []error{nil, down, down} {
		if _, err := m.ListKeys(ctx, 1); !errors.Is(err, want) {
			t.Errorf("list keys call %d: %v, want %v", i+1, err, want)
		}
	}
	m.FailID["GetAccount"] = 7
	if _, err := m.GetAccount(ctx, 1); !errors.Is(err, apiaccess.ErrNotFound) {
		t.Errorf("another id: %v", err)
	}
	if _, err := m.GetAccount(ctx, 7); !errors.Is(err, down) {
		t.Errorf("aimed id: %v", err)
	}
}
