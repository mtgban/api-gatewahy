package config

import (
	"strings"
	"testing"
)

const goodJSON = `{
  "gateway_email": "gateway@mtgban.com",
  "apiaccess_config": {"host": "db", "port": 5432, "user": "u", "password": "p", "dbname": "apiaccess"},
  "games": {
    "magic": {"upstream": "https://www.mtgban.com", "secret": "s1"},
    "pokemon": {"upstream": "https://pokemon.mtgban.com", "secret": "s2"}
  },
  "known_stores": ["TCG", "CK"]
}`

func TestParseAppliesDefaults(t *testing.T) {
	c, err := Parse(strings.NewReader(goodJSON))
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != "8080" || c.InstanceName != "api-gatewahy" || c.Link != "http://www.mtgban.com" || c.CacheTTLSeconds != 60 ||
		c.PerKeyRequestsPerSec != 10 || c.PerKeyBurst != 5 || c.UpstreamTimeoutSeconds != 300 ||
		c.ShutdownGraceSeconds != 60 || c.StaleGraceSeconds != 600 || c.UsageRetentionDays != 395 ||
		c.ClientIPHeader != DefaultClientIPHeader {
		t.Errorf("defaults not applied: %+v", c)
	}
}

func TestParseKeepsEmptyClientIPHeader(t *testing.T) {
	c, err := Parse(strings.NewReader(strings.Replace(goodJSON, `{`, `{"client_ip_header": "",`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if c.ClientIPHeader != "" {
		t.Errorf("client_ip_header %q, want the configured empty value", c.ClientIPHeader)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name, json, want string
	}{
		{"no games", `{"gateway_email":"g@x","apiaccess_config":{"host":"h"},"games":{}}`, "no games"},
		{"empty secret", `{"gateway_email":"g@x","apiaccess_config":{"host":"h"},"games":{"magic":{"upstream":"https://x","secret":""}}}`, "magic: empty secret"},
		{"bad upstream", `{"gateway_email":"g@x","apiaccess_config":{"host":"h"},"games":{"magic":{"upstream":"x","secret":"s"}}}`, "magic: upstream"},
		{"no email", `{"apiaccess_config":{"host":"h"},"games":{"magic":{"upstream":"https://x","secret":"s"}}}`, "gateway_email"},
		{"no db", `{"gateway_email":"g@x","games":{"magic":{"upstream":"https://x","secret":"s"}}}`, "apiaccess_config"},
		{"uppercase game", `{"gateway_email":"g@x","apiaccess_config":{"host":"h"},"games":{"Magic":{"upstream":"https://x","secret":"s"}}}`, "games.Magic: name"},
		{"hyphenated game", `{"gateway_email":"g@x","apiaccess_config":{"host":"h"},"games":{"magic-tcg":{"upstream":"https://x","secret":"s"}}}`, "games.magic-tcg: name"},
	}
	for _, c := range cases {
		_, err := Parse(strings.NewReader(c.json))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want containing %q", c.name, err, c.want)
		}
	}
}

func TestParseRejectsMalformedJSON(t *testing.T) {
	if _, err := Parse(strings.NewReader("{")); err == nil {
		t.Error("expected error")
	}
}

func TestParseStripeDefaults(t *testing.T) {
	c, err := Parse(strings.NewReader(goodJSON))
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicURL != "https://api.mtgban.com" || c.Stripe.GraceDays != 10 ||
		c.Stripe.SuccessPath != "/checkout/success" || c.Stripe.CancelPath != "/checkout/cancel" {
		t.Errorf("stripe defaults not applied: %+v %+v", c.PublicURL, c.Stripe)
	}
	success, cancel := c.CheckoutURLs()
	if success != "https://api.mtgban.com/checkout/success" || cancel != "https://api.mtgban.com/checkout/cancel" {
		t.Errorf("checkout urls %q %q", success, cancel)
	}
}

func TestParseKeepsZeroGraceDays(t *testing.T) {
	c, err := Parse(strings.NewReader(strings.Replace(goodJSON, `{`, `{"stripe": {"grace_days": 0},`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if c.Stripe.GraceDays != 0 {
		t.Errorf("grace_days %d, want the configured 0", c.Stripe.GraceDays)
	}
}

func TestCheckoutURLsTrimSlash(t *testing.T) {
	c, err := Parse(strings.NewReader(strings.Replace(goodJSON, `{`, `{"public_url": "https://gw.example.com/",`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if success, _ := c.CheckoutURLs(); success != "https://gw.example.com/checkout/success" {
		t.Errorf("success url %q", success)
	}
}

func TestPortalConfigDefaultsAndValidation(t *testing.T) {
	c, err := Parse(strings.NewReader(goodJSON))
	if err != nil {
		t.Fatal(err)
	}
	if c.PricingURL != "https://mtgban.com/api-plans" || c.Mail.From != "MTGBAN <no-reply@mtgban.com>" || c.TrialDays != 15 || c.LoginLinksPerHour != 5 {
		t.Errorf("defaults %+v %+v", c.PricingURL, c.Mail)
	}
	withAdmins := strings.Replace(goodJSON, `"gateway_email"`, `"admin_emails": [" Ops@MTGBAN.com "], "trial_days": 7, "gateway_email"`, 1)
	c, err = Parse(strings.NewReader(withAdmins))
	if err != nil || len(c.AdminEmails) != 1 || c.AdminEmails[0] != "ops@mtgban.com" || c.TrialDays != 7 {
		t.Errorf("%+v %v", c, err)
	}
	for _, bad := range []string{`"pricing_url": "not a url"`, `"trial_days": -1`, `"login_links_per_hour": -2`, `"mail": {"from": "nobody"}`} {
		src := strings.Replace(goodJSON, `"gateway_email"`, bad+`, "gateway_email"`, 1)
		if _, err := Parse(strings.NewReader(src)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestValidateRejectsStripeFields(t *testing.T) {
	cases := []struct {
		name, extra, want string
	}{
		{"relative public_url", `"public_url": "api.mtgban.com",`, "public_url"},
		{"negative grace", `"stripe": {"grace_days": -1},`, "grace_days"},
		{"path without slash", `"stripe": {"success_path": "done"},`, "success_path"},
		{"cancel without slash", `"stripe": {"cancel_path": "nope"},`, "cancel_path"},
		{"success equals cancel", `"stripe": {"success_path": "/x", "cancel_path": "/x"},`, "must differ"},
		{"brace in success", `"stripe": {"success_path": "/checkout/{id}"},`, "must not contain braces"},
		{"brace in cancel", `"stripe": {"cancel_path": "/checkout/{id}"},`, "must not contain braces"},
		{"success is root", `"stripe": {"success_path": "/"},`, "must not use a reserved path"},
		{"success is healthz", `"stripe": {"success_path": "/healthz"},`, "must not use a reserved path"},
		{"success is webhook", `"stripe": {"success_path": "/stripe/webhook"},`, "must not use a reserved path"},
		{"success is v1", `"stripe": {"success_path": "/v1/games.json"},`, "must not use a reserved path"},
	}
	for _, c := range cases {
		_, err := Parse(strings.NewReader(strings.Replace(goodJSON, `{`, `{`+c.extra, 1)))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want containing %q", c.name, err, c.want)
		}
	}
}
