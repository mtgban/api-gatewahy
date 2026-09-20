package apiaccess

import (
	"context"
	"testing"
)

func TestStripeEventLedger(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()

	fresh, err := c.BeginStripeEvent(ctx, "evt_1", "invoice.paid")
	if err != nil || !fresh {
		t.Fatalf("first begin: %v %v", fresh, err)
	}
	fresh, err = c.BeginStripeEvent(ctx, "evt_1", "invoice.paid")
	if err != nil || fresh {
		t.Errorf("in-flight event claimed again: %v %v", fresh, err)
	}
	if err := c.FinishStripeEvent(ctx, "evt_1"); err != nil {
		t.Fatal(err)
	}
	fresh, err = c.BeginStripeEvent(ctx, "evt_1", "invoice.paid")
	if err != nil || fresh {
		t.Errorf("finished event claimed again: %v %v", fresh, err)
	}
	if err := c.DeleteStripeEvent(ctx, "evt_1"); err != nil {
		t.Fatal(err)
	}
	fresh, err = c.BeginStripeEvent(ctx, "evt_1", "invoice.paid")
	if err != nil || !fresh {
		t.Errorf("deleted event not claimable: %v %v", fresh, err)
	}
}

func TestStripeEventStaleClaimIsReclaimed(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	if _, err := c.BeginStripeEvent(ctx, "evt_stale", "invoice.paid"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.ExecContext(ctx, `UPDATE stripe_events SET received_at = now() - interval '10 minutes' WHERE id = 'evt_stale'`); err != nil {
		t.Fatal(err)
	}
	fresh, err := c.BeginStripeEvent(ctx, "evt_stale", "invoice.paid")
	if err != nil || !fresh {
		t.Errorf("stale claim not reclaimed: %v %v", fresh, err)
	}
	if err := c.FinishStripeEvent(ctx, "evt_stale"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.ExecContext(ctx, `UPDATE stripe_events SET received_at = now() - interval '10 minutes' WHERE id = 'evt_stale'`); err != nil {
		t.Fatal(err)
	}
	fresh, err = c.BeginStripeEvent(ctx, "evt_stale", "invoice.paid")
	if err != nil || fresh {
		t.Errorf("finished old event reclaimed: %v %v", fresh, err)
	}
}
