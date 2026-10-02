package apiaccess

import (
	"context"
	"errors"
	"testing"

	"github.com/lib/pq"
)

// checkViolation reports whether err is a CHECK failure on constraint.
func checkViolation(err error, constraint string) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23514" && pqErr.Constraint == constraint
}

func TestSchemaRejectsUnknownValues(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	a, err := c.CreateAccount(ctx, "checks@example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	const ent = `INSERT INTO entitlements (account_id, source, games, store_scope, modes, status) VALUES ($1, $2, '{magic}', 'ALL_ACCESS', '{retail}', $3)`
	cases := []struct {
		name, constraint, query string
		args                    []any
	}{
		{"account status", "accounts_status_check", `INSERT INTO accounts (email, status) VALUES ('bad-status@example.com', 'deleted')`, nil},
		{"padded email", "accounts_email_normalized", `INSERT INTO accounts (email) VALUES (' pad@example.com')`, nil},
		{"upper email", "accounts_email_normalized", `INSERT INTO accounts (email) VALUES ('Upper@Example.com')`, nil},
		{"entitlement status", "entitlements_status_check", ent, []any{a.ID, "manual", "paused"}},
		{"entitlement source", "entitlements_source_check", ent, []any{a.ID, "paypal", "active"}},
		{"key kind", "api_keys_kind_check", `INSERT INTO api_keys (account_id, key_hash, prefix, kind) VALUES ($1, 'hash-bad-kind', 'ban_test_x', 'ban_test')`, []any{a.ID}},
	}
	for _, tc := range cases {
		if _, err := c.db.ExecContext(ctx, tc.query, tc.args...); !checkViolation(err, tc.constraint) {
			t.Errorf("%s: err %v, want a %s violation", tc.name, err, tc.constraint)
		}
	}
}

func TestSchemaAcceptsKnownValues(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	a, err := c.CreateAccount(ctx, " Fine@Example.com ", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetAccountStatus(ctx, a.ID, AccountSuspended); err != nil {
		t.Fatal(err)
	}
	for _, src := range []Source{SourceStripe, SourceManual, SourceTrial} {
		e := Entitlement{AccountID: a.ID, Source: src, Games: []string{"magic"}, StoreScope: ScopeAll, Modes: []string{"retail"}}
		if src == SourceStripe {
			// EndEntitlement refuses a Stripe row, so the upsert writes it ended.
			e.ExternalRef, e.Status = "sub_checks", EntitlementEnded
			if _, err := c.UpsertStripeEntitlement(ctx, e); err != nil {
				t.Fatalf("%s: %v", src, err)
			}
			continue
		}
		got, err := c.AddEntitlement(ctx, e)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		if _, err := c.EndEntitlement(ctx, got.ID, 0, got.ValidFrom); err != nil {
			t.Fatalf("end %s: %v", src, err)
		}
	}
	for _, kind := range []KeyKind{KeyLive, KeyDemo} {
		if _, _, err := c.CreateKey(ctx, a.ID, "", kind); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
	}
}

// Migration 2 lowercases and trims emails stored before the check existed.
func TestMigrationNormalizesStoredEmails(t *testing.T) {
	db := freshDB(t)
	ctx := context.Background()
	if err := applyMigrations(ctx, db, migrations[:1]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO accounts (email) VALUES (' Old@Example.COM ')`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var email string
	if err := db.QueryRow(`SELECT email FROM accounts`).Scan(&email); err != nil || email != "old@example.com" {
		t.Errorf("email %q %v, want old@example.com", email, err)
	}
}
