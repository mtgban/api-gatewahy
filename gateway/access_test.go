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
	return apiaccess.Entitlement{Status: "active", ValidFrom: past, Games: games, StoreScope: scope, Modes: modes}
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
			Access{"BASE_ACCESS", []string{"retail", "buylist"}}, true},
		{"all wins", []apiaccess.Entitlement{ent([]string{"magic"}, "BASE_ACCESS", "retail"), ent([]string{"magic"}, "ALL_ACCESS", "buylist")}, "magic",
			Access{"ALL_ACCESS", []string{"retail", "buylist"}}, true},
		{"base beats list", []apiaccess.Entitlement{ent([]string{"magic"}, "CK,TCG", "retail"), ent([]string{"magic"}, "BASE_ACCESS", "retail")}, "magic",
			Access{"BASE_ACCESS", []string{"retail"}}, true},
		{"lists union", []apiaccess.Entitlement{ent([]string{"magic"}, "CK,TCG", "retail"), ent([]string{"magic"}, "SCG,TCG", "sealed")}, "magic",
			Access{"CK,SCG,TCG", []string{"retail", "sealed"}}, true},
		{"expired ignored", []apiaccess.Entitlement{expired, ent([]string{"magic"}, "BASE_ACCESS", "retail")}, "magic",
			Access{"BASE_ACCESS", []string{"retail"}}, true},
		{"only expired", []apiaccess.Entitlement{expired}, "magic", Access{}, false},
	}
	for _, c := range cases {
		got, ok := Resolve(c.ents, c.game, now)
		if ok != c.found || got.StoreScope != c.want.StoreScope || !slices.Equal(got.Modes, c.want.Modes) {
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
