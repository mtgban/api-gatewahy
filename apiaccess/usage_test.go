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

func TestUsageByKeyAndTopPaths(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	a, _ := c.CreateAccount(ctx, "u@example.com", "")
	_, k1, _ := c.CreateKey(ctx, a.ID, "one", KeyLive)
	_, k2, _ := c.CreateKey(ctx, a.ID, "two", KeyLive)
	day := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	rows := []Usage{
		{Ts: day, KeyID: k1.ID, AccountID: a.ID, Game: "magic", Path: "/retail/ZEN.json", Status: 200},
		{Ts: day.Add(time.Hour), KeyID: k1.ID, AccountID: a.ID, Game: "magic", Path: "/retail/ZEN.json", Status: 200},
		{Ts: day.Add(2 * time.Hour), KeyID: k1.ID, AccountID: a.ID, Game: "magic", Path: "/sets.json", Status: 404},
		{Ts: day.Add(24 * time.Hour), KeyID: k1.ID, AccountID: a.ID, Game: "magic", Path: "/sets.json", Status: 200},
		{Ts: day, KeyID: k2.ID, AccountID: a.ID, Game: "magic", Path: "/stores.json", Status: 200},
	}
	if err := c.InsertUsage(ctx, rows); err != nil {
		t.Fatal(err)
	}
	got, err := c.UsageByKey(ctx, day.Add(-time.Hour), day.Add(48*time.Hour), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].KeyID != k1.ID || got[0].Requests != 3 || got[0].Errors != 1 || got[1].Requests != 1 || got[2].KeyID != k2.ID || got[0].Label != "one" {
		t.Errorf("by key: %+v", got)
	}
	paths, err := c.TopPaths(ctx, day.Add(-time.Hour), day.Add(48*time.Hour), k1.ID, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || paths[0].Path != "/retail/ZEN.json" || paths[0].Requests != 2 || paths[1].Errors != 1 {
		t.Errorf("paths: %+v", paths)
	}
}
