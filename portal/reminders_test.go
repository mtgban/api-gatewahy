package portal

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestSendTrialRemindersOnce(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	a, _ := ts.store.GetOrCreateAccount(ctx, "ann@example.com", "")
	b, _ := ts.store.GetOrCreateAccount(ctx, "bob@example.com", "")
	c, _ := ts.store.GetOrCreateAccount(ctx, "carl@example.com", "")
	ts.store.SetAccountStatus(ctx, c.ID, "suspended")
	ts.store.CreateTrial(ctx, "ann@example.com", a.ID, ts.now.Add(2*24*time.Hour), ts.now.Add(-trialCooldown))
	ts.store.CreateTrial(ctx, "bob@example.com", b.ID, ts.now.Add(10*24*time.Hour), ts.now.Add(-trialCooldown))
	ts.store.CreateTrial(ctx, "carl@example.com", c.ID, ts.now.Add(2*24*time.Hour), ts.now.Add(-trialCooldown))

	ts.SendTrialReminders(ctx, ts.now)
	if got := strings.Count(ts.mail.String(), "trial ends soon"); got != 1 || !strings.Contains(ts.mail.String(), "ann@example.com") {
		t.Fatalf("first run sent %d:\n%s", got, ts.mail.String())
	}
	if strings.Contains(ts.mail.String(), "carl@example.com") {
		t.Error("suspended account's trial reminder was mailed")
	}
	ts.mail.Reset()
	ts.SendTrialReminders(ctx, ts.now)
	if ts.mail.Len() != 0 {
		t.Errorf("second run resent:\n%s", ts.mail.String())
	}
	ts.SendTrialReminders(ctx, ts.now.Add(8*24*time.Hour))
	if !strings.Contains(ts.mail.String(), "bob@example.com") {
		t.Error("bob's reminder never went out")
	}
}
