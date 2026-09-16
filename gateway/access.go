// Package gateway decides and forwards API requests.
package gateway

import (
	"slices"
	"strings"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
)

// Access is what an account may do for one game.
type Access struct {
	StoreScope string
	Modes      []string
}

// HasMode reports whether m is allowed.
func (a Access) HasMode(m string) bool {
	return slices.Contains(a.Modes, m)
}

// Resolve unions the active entitlements that name game.
func Resolve(ents []apiaccess.Entitlement, game string, now time.Time) (Access, bool) {
	var (
		found    bool
		sawAll   bool
		sawBase  bool
		stores   []string
		modeSeen = map[string]bool{}
	)
	for _, e := range ents {
		if !e.ActiveAt(now) || !slices.Contains(e.Games, game) {
			continue
		}
		found = true
		switch e.StoreScope {
		case apiaccess.ScopeAll:
			sawAll = true
		case apiaccess.ScopeBase:
			sawBase = true
		default:
			for _, s := range strings.Split(e.StoreScope, ",") {
				if s != "" && !slices.Contains(stores, s) {
					stores = append(stores, s)
				}
			}
		}
		for _, m := range e.Modes {
			modeSeen[m] = true
		}
	}
	if !found {
		return Access{}, false
	}
	a := Access{}
	switch {
	case sawAll:
		a.StoreScope = apiaccess.ScopeAll
	case sawBase:
		a.StoreScope = apiaccess.ScopeBase
	default:
		slices.Sort(stores)
		a.StoreScope = strings.Join(stores, ",")
	}
	for _, m := range apiaccess.ValidModes {
		if modeSeen[m] {
			a.Modes = append(a.Modes, m)
		}
	}
	return a, true
}
