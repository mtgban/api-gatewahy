package gateway

import (
	"slices"
	"testing"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
)

var (
	now  = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	past = now.Add(-24 * time.Hour)
)

func ent(games []string, scope string, modes ...string) apiaccess.Entitlement {
	return apiaccess.Entitlement{Status: apiaccess.EntitlementActive, ValidFrom: past, Games: games, StoreScope: scope, Modes: modes}
}

// hist is an active history row for games with a lookback.
func hist(games []string, days int) apiaccess.Entitlement {
	return apiaccess.Entitlement{Status: apiaccess.EntitlementActive, ValidFrom: past, Games: games, Modes: []string{"history"}, LookbackDays: days}
}

func TestResolve(t *testing.T) {
	expired := ent([]string{"magic"}, "ALL_ACCESS", "sealed")
	expired.ValidUntil = &past

	cases := []struct {
		name  string
		ents  []apiaccess.Entitlement
		game  string
		want  Access
		found bool
	}{
		{"none", nil, "magic", Access{}, false},
		{"wrong game", []apiaccess.Entitlement{ent([]string{"pokemon"}, "ALL_ACCESS", "retail")}, "magic", Access{}, false},
		{"single", []apiaccess.Entitlement{ent([]string{"magic"}, "BASE_ACCESS", "retail", "buylist")}, "magic",
			Access{StoreScope: "BASE_ACCESS", Modes: []string{"retail", "buylist"}}, true},
		{"all wins", []apiaccess.Entitlement{ent([]string{"magic"}, "BASE_ACCESS", "retail"), ent([]string{"magic"}, "ALL_ACCESS", "buylist")}, "magic",
			Access{StoreScope: "ALL_ACCESS", Modes: []string{"retail", "buylist"}}, true},
		{"base beats list", []apiaccess.Entitlement{ent([]string{"magic"}, "CK,TCG", "retail"), ent([]string{"magic"}, "BASE_ACCESS", "retail")}, "magic",
			Access{StoreScope: "BASE_ACCESS", Modes: []string{"retail"}}, true},
		{"lists union", []apiaccess.Entitlement{ent([]string{"magic"}, "CK,TCG", "retail"), ent([]string{"magic"}, "SCG,TCG", "sealed")}, "magic",
			Access{StoreScope: "CK,SCG,TCG", Modes: []string{"retail", "sealed"}}, true},
		{"expired ignored", []apiaccess.Entitlement{expired, ent([]string{"magic"}, "BASE_ACCESS", "retail")}, "magic",
			Access{StoreScope: "BASE_ACCESS", Modes: []string{"retail"}}, true},
		{"only expired", []apiaccess.Entitlement{expired}, "magic", Access{}, false},
		{"history alone", []apiaccess.Entitlement{hist([]string{"magic"}, 365)}, "magic",
			Access{StoreScope: "", Modes: []string{"history"}, LookbackDays: 365}, true},
		{"history beside a plan", []apiaccess.Entitlement{ent([]string{"magic"}, "BASE_ACCESS", "retail"), hist([]string{"magic"}, 30)}, "magic",
			Access{StoreScope: "BASE_ACCESS", Modes: []string{"retail", "history"}, LookbackDays: 30}, true},
		{"max lookback", []apiaccess.Entitlement{hist([]string{"magic"}, 30), hist([]string{"magic"}, 365)}, "magic",
			Access{StoreScope: "", Modes: []string{"history"}, LookbackDays: 365}, true},
		{"history for another game", []apiaccess.Entitlement{ent([]string{"magic"}, "CK", "retail"), hist([]string{"pokemon"}, 365)}, "magic",
			Access{StoreScope: "CK", Modes: []string{"retail"}, LookbackDays: 0}, true},
	}
	for _, c := range cases {
		got, ok := Resolve(c.ents, c.game, now)
		if ok != c.found || got.StoreScope != c.want.StoreScope || !slices.Equal(got.Modes, c.want.Modes) || got.LookbackDays != c.want.LookbackDays {
			t.Errorf("%s: got %+v %v, want %+v %v", c.name, got, ok, c.want, c.found)
		}
	}
}

func TestHasMode(t *testing.T) {
	a := Access{Modes: []string{"retail"}}
	if !a.HasMode("retail") || a.HasMode("sealed") {
		t.Error("HasMode wrong")
	}
}
