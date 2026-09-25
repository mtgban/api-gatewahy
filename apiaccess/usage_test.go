package apiaccess

import (
	"context"
	"testing"
	"time"
)

func TestUsageInsertSummarizePrune(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	a, _ := c.CreateAccount(ctx, "u@example.com", "")
	_, k, _ := c.CreateKey(ctx, a.ID, "", KeyLive)
	now := time.Now().UTC()

	rows := []Usage{
		{Ts: now, KeyID: k.ID, AccountID: a.ID, Game: "magic", Path: "/api/mtgban/retail.json", Status: 200, Bytes: 1000, DurationMS: 50, ClientIP: "203.0.113.5"},
		{Ts: now, KeyID: k.ID, AccountID: a.ID, Game: "magic", Path: "/api/mtgban/buylist.json", Status: 502, Bytes: 40, DurationMS: 5},
		{Ts: now.Add(-48 * time.Hour), KeyID: k.ID, AccountID: a.ID, Game: "pokemon", Path: "/api/mtgban/sets.json", Status: 200, Bytes: 10, DurationMS: 1},
	}
	if err := c.InsertUsage(ctx, rows); err != nil {
		t.Fatal(err)
	}

	sum, err := c.SummarizeUsage(ctx, now.Add(-time.Hour), now.Add(time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum) != 1 || sum[0].Game != "magic" || sum[0].Requests != 2 || sum[0].Bytes != 1040 || sum[0].Errors != 1 || sum[0].Email != "u@example.com" {
		t.Errorf("summary %+v", sum)
	}

	n, err := c.PruneUsage(ctx, now.Add(-24*time.Hour))
	if err != nil || n != 1 {
		t.Errorf("prune %d %v", n, err)
	}
}

func TestInsertUsageEmpty(t *testing.T) {
	c := testClient(t)
	if err := c.InsertUsage(context.Background(), nil); err != nil {
		t.Error(err)
	}
}

func TestInsertUsageDropsUnparseableIP(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	a, _ := c.CreateAccount(ctx, "ip@example.com", "")
	_, k, _ := c.CreateKey(ctx, a.ID, "", KeyLive)
	now := time.Now().UTC()

	rows := []Usage{
		{Ts: now, KeyID: k.ID, AccountID: a.ID, Game: "magic", Path: "/api/mtgban/retail.json", Status: 200, ClientIP: "203.0.113.5"},
		{Ts: now, KeyID: k.ID, AccountID: a.ID, Game: "magic", Path: "/api/mtgban/buylist.json", Status: 200, ClientIP: "potato"},
		{Ts: now, KeyID: k.ID, AccountID: a.ID, Game: "magic", Path: "/api/mtgban/sets.json", Status: 200, ClientIP: "fe80::1%eth0"},
	}
	if err := c.InsertUsage(ctx, rows); err != nil {
		t.Fatal(err)
	}
	var total, withIP int
	if err := c.db.QueryRowContext(ctx,
		`SELECT count(*), count(client_ip) FROM usage WHERE account_id = $1`, a.ID).Scan(&total, &withIP); err != nil {
		t.Fatal(err)
	}
	if total != 3 || withIP != 1 {
		t.Errorf("stored %d rows with %d addresses, want 3 and 1", total, withIP)
	}
}
