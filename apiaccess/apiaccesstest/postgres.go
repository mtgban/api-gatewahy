package apiaccesstest

import (
	"context"
	"database/sql"
	"testing"
)

// tables are the ones testdb_test.go clears, children before parents; keep them in step.
var tables = []string{"usage", "entitlements", "api_keys", "invites", "stripe_events", "handoff_nonces", "magic_links", "trials", "admin_actions", "accounts"}

// ResetPostgres empties every apiaccess table in the database at dsn.
func ResetPostgres(t testing.TB, dsn string) {
	t.Helper()
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, table := range tables {
		if _, err := db.ExecContext(context.Background(), "DELETE FROM "+table); err != nil {
			t.Errorf("reset %s: %v", table, err)
		}
	}
}
