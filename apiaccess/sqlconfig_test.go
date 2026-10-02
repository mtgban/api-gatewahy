package apiaccess_test

import (
	"testing"

	"github.com/lib/pq"
	"github.com/mtgban/api-gatewahy/apiaccess"
)

func TestSQLConfigDSNQuotesPasswords(t *testing.T) {
	for _, password := range []string{"", "has a space", `o'brien\'s`} {
		cfg := apiaccess.SQLConfig{Host: "db", Port: 5432, User: "u", Password: password, DBName: "apiaccess"}
		dsn := cfg.DSN()
		if _, err := pq.NewConnector(dsn); err != nil {
			t.Fatalf("password %q: NewConnector: %v", password, err)
		}
		parsed, err := pq.NewConfig(dsn)
		if err != nil {
			t.Fatalf("password %q: NewConfig: %v", password, err)
		}
		if parsed.Password != password {
			t.Errorf("password %q round-tripped as %q", password, parsed.Password)
		}
	}
}

func TestSQLConfigDSNDefaultsSSLMode(t *testing.T) {
	cfg := apiaccess.SQLConfig{Host: "db", Port: 5432, User: "u", DBName: "apiaccess"}
	parsed, err := pq.NewConfig(cfg.DSN())
	if err != nil {
		t.Fatal(err)
	}
	if parsed.SSLMode != "disable" {
		t.Errorf("sslmode %q, want disable", parsed.SSLMode)
	}
}

func TestSQLConfigDSNRoundTripsEveryField(t *testing.T) {
	cfg := apiaccess.SQLConfig{Host: "db host", Port: 6432, User: `o'brien`, Password: `p\ss`, DBName: "api access", SSLMode: "require"}
	parsed, err := pq.NewConfig(cfg.DSN())
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Host != cfg.Host || parsed.Port != 6432 || parsed.User != cfg.User || parsed.Password != cfg.Password || parsed.Database != cfg.DBName || string(parsed.SSLMode) != cfg.SSLMode {
		t.Errorf("round trip: %+v", parsed)
	}
}
