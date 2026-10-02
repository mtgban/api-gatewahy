package apiaccess

import (
	"context"
	"testing"
	"time"
)

// remaining counts the rows of table matching where.
func remaining(t *testing.T, c *Client, table, where string, args ...any) int {
	t.Helper()
	var n int
	if err := c.db.QueryRow(`SELECT count(*) FROM `+table+` WHERE `+where, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPruneStripeEvents(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	if _, err := c.BeginStripeEvent(ctx, "evt_recent", "invoice.paid"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`INSERT INTO stripe_events (id, type, received_at, processed_at) VALUES ('evt_old', 'invoice.paid', now() - interval '40 days', now() - interval '40 days')`); err != nil {
		t.Fatal(err)
	}
	n, err := c.PruneStripeEvents(ctx, time.Now().AddDate(0, 0, -30))
	if err != nil || n != 1 {
		t.Fatalf("pruned %d %v, want 1", n, err)
	}
	if remaining(t, c, "stripe_events", "id = 'evt_recent'") != 1 || remaining(t, c, "stripe_events", "id = 'evt_old'") != 0 {
		t.Error("wrong stripe event pruned")
	}
}

func TestPruneInvites(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	for _, note := range []string{"live", "lapsed", "old", "old used"} {
		if _, _, err := c.CreateInvite(ctx, "quarterly", "", 7*24*time.Hour, note); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.db.Exec(`UPDATE invites SET expires_at = now() - interval '1 day' WHERE note = 'lapsed'`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`UPDATE invites SET expires_at = now() - interval '40 days', used_at = CASE WHEN note = 'old used' THEN now() - interval '45 days' END WHERE note LIKE 'old%'`); err != nil {
		t.Fatal(err)
	}
	n, err := c.PruneInvites(ctx, time.Now().AddDate(0, 0, -30))
	if err != nil || n != 2 {
		t.Fatalf("pruned %d %v, want 2", n, err)
	}
	if got := remaining(t, c, "invites", "note IN ('live', 'lapsed')"); got != 2 || remaining(t, c, "invites", "true") != 2 {
		t.Errorf("kept %d of live and lapsed, want both and nothing else", got)
	}
}

func TestPruneAdminActions(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	for _, action := range []string{"recent", "old"} {
		if err := c.RecordAdminAction(ctx, "admin@example.com", action, 0, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.db.Exec(`UPDATE admin_actions SET at = now() - interval '100 days' WHERE action = 'old'`); err != nil {
		t.Fatal(err)
	}
	n, err := c.PruneAdminActions(ctx, time.Now().AddDate(0, 0, -90))
	if err != nil || n != 1 {
		t.Fatalf("pruned %d %v, want 1", n, err)
	}
	if remaining(t, c, "admin_actions", "action = 'recent'") != 1 || remaining(t, c, "admin_actions", "action = 'old'") != 0 {
		t.Error("wrong admin action pruned")
	}
}
