package apiaccess

import (
	"context"
	"testing"
	"time"
)

func TestListDemoAccess(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	trial, _ := c.CreateAccount(ctx, "trial@example.com", "")
	paid, _ := c.CreateAccount(ctx, "paid@example.com", "")
	manual, _ := c.CreateAccount(ctx, "manual@example.com", "")
	ends := time.Now().Add(15 * 24 * time.Hour)
	if _, err := c.AddEntitlement(ctx, Entitlement{AccountID: trial.ID, Source: "trial", Games: []string{"magic"}, StoreScope: "ALL_ACCESS", Modes: []string{"retail"}, ValidUntil: &ends}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateTrial(ctx, "patron@example.com", trial.ID, ends, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddEntitlement(ctx, Entitlement{AccountID: paid.ID, Source: "stripe", Games: []string{"magic"}, StoreScope: "ALL_ACCESS", Modes: []string{"retail"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddEntitlement(ctx, Entitlement{AccountID: manual.ID, Source: "manual", Games: []string{"magic"}, StoreScope: "BASE_ACCESS", Modes: []string{"retail"}, Note: "zoho invoice 12"}); err != nil {
		t.Fatal(err)
	}
	// A past trial on the manual account must not become its requester.
	if _, err := c.CreateTrial(ctx, "lapsed@example.com", manual.ID, ends, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"a", "b"} {
		if _, _, err := c.CreateKey(ctx, trial.ID, label, KeyDemo); err != nil {
			t.Fatal(err)
		}
	}
	// A revoked key must not count toward Keys.
	_, gone, err := c.CreateKey(ctx, trial.ID, "c", KeyDemo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RevokeKey(ctx, gone.ID, trial.ID); err != nil {
		t.Fatal(err)
	}
	// An entitlement that already ran out must not appear.
	past := time.Now().Add(-time.Hour)
	if _, err := c.AddEntitlement(ctx, Entitlement{AccountID: paid.ID, Source: "trial", Games: []string{"magic"}, StoreScope: "ALL_ACCESS", Modes: []string{"retail"}, ValidUntil: &past}); err != nil {
		t.Fatal(err)
	}
	rows, err := c.ListDemoAccess(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows %+v", rows)
	}
	byEmail := map[string]DemoAccess{}
	for _, r := range rows {
		byEmail[r.Email] = r
	}
	if r := byEmail["trial@example.com"]; r.Source != "trial" || r.Requester != "patron@example.com" || r.Keys != 2 || r.EndsAt == nil {
		t.Errorf("trial row %+v", r)
	}
	if r := byEmail["manual@example.com"]; r.Source != "manual" || r.Note != "zoho invoice 12" || r.Keys != 0 || r.Requester != "" {
		t.Errorf("manual row %+v", r)
	}
}
