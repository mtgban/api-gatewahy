package portal

import (
	"context"
	"net/url"
	"slices"
	"strings"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/api-gatewahy/billing"
	"github.com/mtgban/mtgban-website/apiproductlist"
)

// listValues accepts repeated fields and comma-joined values alike.
func listValues(v url.Values, key string) []string {
	var out []string
	for _, raw := range v[key] {
		for _, part := range strings.Split(raw, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// planFromValues reads the configurator's fields, or the pending cookie's copy of them.
func planFromValues(v url.Values) billing.Plan {
	return billing.Plan{Package: v.Get("package"), Interval: v.Get("interval"), Games: listValues(v, "games"), Stores: listValues(v, "stores")}
}

// planValues is the inverse, for hidden fields and the pending cookie.
func planValues(p billing.Plan) url.Values {
	v := url.Values{}
	v.Set("package", p.Package)
	v.Set("interval", p.Interval)
	v.Set("games", strings.Join(p.Games, ","))
	v.Set("stores", strings.Join(p.Stores, ","))
	return v
}

// entitlementView is one entitlement in words.
type entitlementView struct {
	Source  string
	Package string
	Games   string
	Stores  string
	Modes   string
	Until   string
	Trial   bool
}

// describeEntitlement renders e for the account and admin pages.
func (s *Server) describeEntitlement(sites *siteLookup, e apiaccess.Entitlement) entitlementView {
	v := entitlementView{Games: strings.Join(e.Games, ", "), Modes: strings.Join(e.Modes, ", ")}
	switch e.Source {
	case "stripe":
		v.Source = "Stripe subscription"
	case "trial":
		v.Source = "Trial"
		v.Trial = true
	default:
		v.Source = "Arranged with MTGBAN"
	}
	scope := e.StoreScope
	switch scope {
	case apiaccess.ScopeAll:
		v.Stores = "every store"
	case apiaccess.ScopeBase:
		v.Stores = "every EU and US store"
	default:
		implied, selectable, _ := sites.families(e.Games)
		v.Stores = storeNames(append(implied, selectable...), strings.Split(scope, ","))
		scope = apiproductlist.StoreScopeExplicit
	}
	for _, p := range s.Catalog.Packages {
		if p.StoreScope == scope {
			v.Package = p.Name
			break
		}
	}
	if e.ValidUntil != nil {
		v.Until = e.ValidUntil.Format("January 2, 2006")
	}
	return v
}

// prefillQuery rebuilds the configurator query for e so the pricing page can
// open with the current plan selected.
func (s *Server) prefillQuery(sites *siteLookup, e apiaccess.Entitlement) url.Values {
	q := url.Values{"change": {"1"}, "games": {strings.Join(e.Games, ",")}}
	scope := e.StoreScope
	if scope != apiaccess.ScopeAll && scope != apiaccess.ScopeBase {
		have := strings.Split(scope, ",")
		// A shorthand no current family owns is dropped.
		var keys []string
		_, selectable, complete := sites.families(e.Games)
		for _, f := range selectable {
			if !slices.Contains(keys, f.Key) && slices.ContainsFunc(f.Shorthands, func(sh string) bool { return slices.Contains(have, sh) }) {
				keys = append(keys, f.Key)
			}
		}
		slices.Sort(keys)
		// With a site unread the list would be narrowed, so the page keeps its own selection.
		if complete {
			q.Set("stores", strings.Join(keys, ","))
		}
		scope = apiproductlist.StoreScopeExplicit
	}
	for _, p := range s.Catalog.Packages {
		if p.StoreScope == scope {
			q.Set("package", p.Key)
			break
		}
	}
	return q
}

// siteLookup reads each game's store list at most once for one page render.
type siteLookup struct {
	s      *Server
	ctx    context.Context
	sites  map[string]billing.SiteStores
	failed map[string]bool
}

func (s *Server) newSiteLookup(ctx context.Context) *siteLookup {
	return &siteLookup{s: s, ctx: ctx, sites: map[string]billing.SiteStores{}, failed: map[string]bool{}}
}

// families gathers the games' implied and selectable families; complete is false when a site could not be read.
func (l *siteLookup) families(games []string) (implied, selectable []billing.StoreFamily, complete bool) {
	complete = true
	for _, g := range games {
		site, ok := l.site(g)
		if !ok {
			complete = false
			continue
		}
		implied = append(implied, site.Implied...)
		selectable = append(selectable, site.Stores...)
	}
	return implied, selectable, complete
}

func (l *siteLookup) site(game string) (billing.SiteStores, bool) {
	if site, ok := l.sites[game]; ok {
		return site, true
	}
	if l.failed[game] || l.s.Stores == nil {
		return billing.SiteStores{}, false
	}
	site, err := l.s.Stores.SiteStores(l.ctx, game)
	if err != nil {
		l.s.logf("stores for %s: %v", game, err)
		l.failed[game] = true
		return billing.SiteStores{}, false
	}
	l.sites[game] = site
	return site, true
}

// storeNames maps backend shorthands to family names, keeping unknown ones as is.
func storeNames(families []billing.StoreFamily, shorthands []string) string {
	var names []string
	for _, sh := range shorthands {
		name := sh
		for _, f := range families {
			if slices.Contains(f.Shorthands, sh) {
				name = f.Name
				break
			}
		}
		if name != "" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return strings.Join(names, ", ")
}
