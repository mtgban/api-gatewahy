package portal

import (
	"fmt"
	"html"
	"time"
)

func magicLinkMail(link string, ttl time.Duration) (subject, text, htmlBody string) {
	minutes := int(ttl.Minutes())
	subject = "Your MTGBAN API sign-in link"
	text = fmt.Sprintf("Sign in to your MTGBAN API account:\n\n%s\n\nThe link works once and expires in %d minutes. If you did not request it, ignore this message.\n", link, minutes)
	htmlBody = fmt.Sprintf(`<p>Sign in to your MTGBAN API account:</p><p><a href="%s">%s</a></p><p>The link works once and expires in %d minutes. If you did not request it, ignore this message.</p>`,
		html.EscapeString(link), html.EscapeString(link), minutes)
	return
}

func keyCreatedMail(prefix, label, accountURL string) (subject, text, htmlBody string) {
	subject = "A new MTGBAN API key was created"
	if label == "" {
		label = "(no label)"
	}
	text = fmt.Sprintf("A new API key starting %s (%s) was just created on your account. If that was not you, revoke it now at %s and contact administrator@mtgban.com.\n", prefix, label, accountURL)
	htmlBody = fmt.Sprintf(`<p>A new API key starting <code>%s</code> (%s) was just created on your account.</p><p>If that was not you, <a href="%s">revoke it now</a> and contact administrator@mtgban.com.</p>`,
		html.EscapeString(prefix), html.EscapeString(label), html.EscapeString(accountURL))
	return
}

func trialStartedMail(endsAt time.Time, accountURL string, days int) (subject, text, htmlBody string) {
	when := endsAt.Format("January 2, 2006")
	subject = "Your MTGBAN API trial has started"
	text = fmt.Sprintf("Your %d-day trial of the MTGBAN API is active until %s, covering every game and every store. Create a key and read the guide at %s\n", days, when, accountURL)
	htmlBody = fmt.Sprintf(`<p>Your %d-day trial of the MTGBAN API is active until <strong>%s</strong>, covering every game and every store.</p><p><a href="%s">Create a key and get started</a>.</p>`,
		days, when, html.EscapeString(accountURL))
	return
}

func trialEndingMail(endsAt time.Time, pricingURL string) (subject, text, htmlBody string) {
	when := endsAt.Format("January 2, 2006")
	subject = "Your MTGBAN API trial ends soon"
	text = fmt.Sprintf("Your MTGBAN API trial ends on %s. Nothing is charged automatically. To keep your access, pick a plan at %s\n", when, pricingURL)
	htmlBody = fmt.Sprintf(`<p>Your MTGBAN API trial ends on <strong>%s</strong>. Nothing is charged automatically.</p><p>To keep your access, <a href="%s">pick a plan</a>.</p>`,
		when, html.EscapeString(pricingURL))
	return
}
