package apiaccess

import (
	"context"
	"testing"
	"time"
)

func TestValidateStoreScope(t *testing.T) {
	known := []string{"TCG", "CK", "SCG"}
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{"ALL_ACCESS", "ALL_ACCESS", false},
		{"BASE_ACCESS", "BASE_ACCESS", false},
		{"DEV_ACCESS", "", true},
		{"ck, tcg,CK", "CK,TCG", false},
		{"TCG,XYZ", "", true},
		{"", "", true},
		{" , ", "", true},
	}
	for _, c := range cases {
		got, err := ValidateStoreScope(c.in, known)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("%q: got %q err %v", c.in, got, err)
		}
	}
}

func TestValidateModes(t *testing.T) {
	got, err := ValidateModes([]string{"sealed", "retail", "retail"})
	if err != nil || len(got) != 2 || got[0] != "retail" || got[1] != "sealed" {
		t.Errorf("got %v err %v", got, err)
	}
	if _, err := ValidateModes([]string{"all"}); err == nil {
		t.Error("all accepted")
	}
	if _, err := ValidateModes(nil); err == nil {
		t.Error("empty accepted")
	}
}

func TestActiveAt(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	until := now.Add(time.Hour)
	past := now.Add(-time.Hour)
	cases := []struct {
		name string
		e    Entitlement
		want bool
	}{
		{"open ended", Entitlement{Status: "active", ValidFrom: past}, true},
		{"future start", Entitlement{Status: "active", ValidFrom: until}, false},
		{"expired", Entitlement{Status: "active", ValidFrom: past, ValidUntil: &past}, false},
		{"until later", Entitlement{Status: "active", ValidFrom: past, ValidUntil: &until}, true},
		{"ended", Entitlement{Status: "ended", ValidFrom: past}, false},
	}
	for _, c := range cases {
		if got := c.e.ActiveAt(now); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}

func TestEntitlementsRoundTrip(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	a, _ := c.CreateAccount(ctx, "e@example.com", "")

	e, err := c.AddEntitlement(ctx, Entitlement{
		AccountID: a.ID, Source: "manual", Games: []string{"magic", "pokemon"},
		StoreScope: "BASE_ACCESS", Modes: []string{"retail", "buylist"}, Note: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.ID == 0 || e.Status != "active" || len(e.Games) != 2 || len(e.Addons) != 0 {
		t.Errorf("added %+v", e)
	}

	list, _ := c.ListEntitlements(ctx, a.ID)
	if len(list) != 1 || list[0].StoreScope != "BASE_ACCESS" {
		t.Errorf("list %+v", list)
	}

	if err := c.EndEntitlement(ctx, e.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	active, _ := c.listEntitlements(ctx, a.ID, true)
	if len(active) != 0 {
		t.Errorf("ended row still active: %+v", active)
	}
	all, _ := c.ListEntitlements(ctx, a.ID)
	if len(all) != 1 || all[0].Status != "ended" || all[0].ValidUntil == nil {
		t.Errorf("ended row wrong: %+v", all)
	}
}

func TestAddEntitlementValidates(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	a, _ := c.CreateAccount(ctx, "v@example.com", "")
	base := Entitlement{AccountID: a.ID, Source: "stripe", Games: []string{"magic"}, StoreScope: "tcg", Modes: []string{"retail"}}

	dev := base
	dev.StoreScope = "DEV_ACCESS"
	if _, err := c.AddEntitlement(ctx, dev); err == nil {
		t.Error("DEV_ACCESS was stored")
	}
	all := base
	all.Modes = []string{"all"}
	if _, err := c.AddEntitlement(ctx, all); err == nil {
		t.Error("mode all was stored")
	}
	empty := base
	empty.StoreScope = ""
	if _, err := c.AddEntitlement(ctx, empty); err == nil {
		t.Error("empty store scope was stored")
	}

	e, err := c.AddEntitlement(ctx, base)
	if err != nil || e.StoreScope != "TCG" {
		t.Fatalf("good grant: %+v %v", e, err)
	}

	c.SetKnownStores([]string{"TCG"})
	unknown := base
	unknown.StoreScope = "XYZ"
	if _, err := c.AddEntitlement(ctx, unknown); err == nil {
		t.Error("unknown store was stored")
	}
}
