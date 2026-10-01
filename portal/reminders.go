package portal

import (
	"context"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
)

// reminderLead is how far before the end the ending-soon mail goes out.
const reminderLead = 3 * 24 * time.Hour

// SendTrialReminders mails every trial ending within reminderLead that was
// not reminded yet, marking each so a restart never sends it twice.
func (s *Server) SendTrialReminders(ctx context.Context, now time.Time) {
	due, err := s.Store.TrialsToRemind(ctx, now, now.Add(reminderLead))
	if err != nil {
		s.logf("trial reminders: %v", err)
		return
	}
	for _, t := range due {
		a, err := s.Store.GetAccount(ctx, t.AccountID)
		if err != nil {
			s.logf("trial reminder %d: account: %v", t.ID, err)
			continue
		}
		if a.Status != "active" {
			continue
		}
		ents, err := s.Store.ListEntitlements(ctx, t.AccountID)
		if err != nil {
			s.logf("trial reminder %d: entitlements: %v", t.ID, err)
			continue
		}
		if apiaccess.HasActiveStripePlan(ents) || trialEntitlementEnded(ents, now) {
			continue
		}
		// Mark before sending, so a send failure cannot repeat the mail on the next run.
		if err := s.Store.MarkTrialReminded(ctx, t.ID, now); err != nil {
			s.logf("trial reminder %d: mark: %v", t.ID, err)
			continue
		}
		subject, text, htmlBody := trialEndingMail(t.EndsAt, s.PricingURL)
		if err := s.Mail.Send(ctx, a.Email, subject, text, htmlBody); err != nil {
			s.logf("trial reminder %d: mail: %v (marked sent, will not retry)", t.ID, err)
		}
	}
}

// trialEntitlementEnded reports whether ents has a trial grant and none of
// them are still active, meaning the trial already ended early.
func trialEntitlementEnded(ents []apiaccess.Entitlement, now time.Time) bool {
	var hasTrial, activeTrial bool
	for _, e := range ents {
		if e.Source != "trial" {
			continue
		}
		hasTrial = true
		if e.ActiveAt(now) {
			activeTrial = true
		}
	}
	return hasTrial && !activeTrial
}
