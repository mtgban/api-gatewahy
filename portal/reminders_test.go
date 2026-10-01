package portal

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/api-gatewahy/mailer"
)

func TestSendTrialRemindersOnce(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	a, _ := ts.store.GetOrCreateAccount(ctx, "ann@example.com", "")
	b, _ := ts.store.GetOrCreateAccount(ctx, "bob@example.com", "")
	c, _ := ts.store.GetOrCreateAccount(ctx, "carl@example.com", "")
	_ = ts.store.SetAccountStatus(ctx, c.ID, "suspended")
	_, _ = ts.store.CreateTrial(ctx, "ann@example.com", ts.now.Add(2*24*time.Hour), ts.now.Add(-trialCooldown), entitlementFor(a.ID, "trial", "ALL_ACCESS"))
	_, _ = ts.store.CreateTrial(ctx, "bob@example.com", ts.now.Add(10*24*time.Hour), ts.now.Add(-trialCooldown), entitlementFor(b.ID, "trial", "ALL_ACCESS"))
	_, _ = ts.store.CreateTrial(ctx, "carl@example.com", ts.now.Add(2*24*time.Hour), ts.now.Add(-trialCooldown), entitlementFor(c.ID, "trial", "ALL_ACCESS"))

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

// TestSendTrialRemindersSkipsActivePlanAndEndedTrial covers item 15: an
// account that already bought, or whose trial ended early, gets no reminder.
func TestSendTrialRemindersSkipsActivePlanAndEndedTrial(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	paid, _ := ts.store.GetOrCreateAccount(ctx, "paid@example.com", "")
	ended, _ := ts.store.GetOrCreateAccount(ctx, "ended@example.com", "")
	_, _ = ts.store.CreateTrial(ctx, "paid@example.com", ts.now.Add(2*24*time.Hour), ts.now.Add(-trialCooldown), entitlementFor(paid.ID, "trial", apiaccess.ScopeAll))
	_, _ = ts.store.CreateTrial(ctx, "ended@example.com", ts.now.Add(2*24*time.Hour), ts.now.Add(-trialCooldown), entitlementFor(ended.ID, "trial", apiaccess.ScopeAll))
	_, _ = ts.store.AddEntitlement(ctx, entitlementFor(paid.ID, "stripe", apiaccess.ScopeBase))
	endedEnts, err := ts.store.ListEntitlements(ctx, ended.ID)
	if err != nil || len(endedEnts) != 1 {
		t.Fatalf("ended account's entitlements: %+v %v", endedEnts, err)
	}
	if _, err := ts.store.EndEntitlement(ctx, endedEnts[0].ID, ended.ID, ts.now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	ts.SendTrialReminders(ctx, ts.now)
	if ts.mail.Len() != 0 {
		t.Fatalf("reminder sent to a skipped account:\n%s", ts.mail.String())
	}
}

// failMailer always fails, to prove a reminder is marked before it is sent.
type failMailer struct{}

func (failMailer) Send(context.Context, string, string, string, string) error {
	return errors.New("smtp down")
}

// TestSendTrialRemindersMarksBeforeSend covers item 15: a mail failure must
// not leave the trial unmarked, or a restart would resend it.
func TestSendTrialRemindersMarksBeforeSend(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	a, _ := ts.store.GetOrCreateAccount(ctx, "dana@example.com", "")
	trial, _ := ts.store.CreateTrial(ctx, "dana@example.com", ts.now.Add(2*24*time.Hour), ts.now.Add(-trialCooldown), entitlementFor(a.ID, "trial", apiaccess.ScopeAll))

	ts.Mail = failMailer{}
	ts.SendTrialReminders(ctx, ts.now)
	if ts.store.trials[trial.ID].ReminderSentAt == nil {
		t.Fatal("reminder not marked although marking happens before the send")
	}

	ts.mail.Reset()
	ts.Mail = &mailer.Log{Out: ts.mail}
	ts.SendTrialReminders(ctx, ts.now)
	if ts.mail.Len() != 0 {
		t.Errorf("reminder resent after a send failure although it was already marked:\n%s", ts.mail.String())
	}
}
