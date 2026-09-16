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
