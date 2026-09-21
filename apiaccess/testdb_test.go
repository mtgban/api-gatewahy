package apiaccess

import (
	"context"
	"os"
	"testing"
)

// testClient opens the scratch database named by APIACCESS_TEST_DSN, or
// skips. DSN form: postgres://user:pass@host:5432/dbname?sslmode=disable
func testClient(t *testing.T) *Client {
	t.Helper()
	dsn := os.Getenv("APIACCESS_TEST_DSN")
	if dsn == "" {
		t.Skip("APIACCESS_TEST_DSN not set")
	}
	c, err := newClientDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		for _, table := range []string{"usage", "entitlements", "api_keys", "invites", "stripe_events", "handoff_nonces", "magic_links", "trials", "admin_actions", "accounts"} {
			if _, err := c.db.ExecContext(ctx, "DELETE FROM "+table); err != nil {
				t.Errorf("cleanup %s: %v", table, err)
			}
		}
		_ = c.Close()
	})
	return c
}
