package portal

import (
	"context"
	"time"
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
		subject, text, htmlBody := trialEndingMail(t.EndsAt, s.PricingURL)
		if err := s.Mail.Send(ctx, a.Email, subject, text, htmlBody); err != nil {
			s.logf("trial reminder %d: mail: %v", t.ID, err)
			continue
		}
		if err := s.Store.MarkTrialReminded(ctx, t.ID, now); err != nil {
			s.logf("trial reminder %d: mark: %v", t.ID, err)
		}
	}
}
