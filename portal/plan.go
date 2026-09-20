package portal

import (
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
func (s *Server) describeEntitlement(e apiaccess.Entitlement) entitlementView {
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
		v.Stores = storeNames(s.Catalog, strings.Split(scope, ","))
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
func prefillQuery(cat *apiproductlist.ProductList, e apiaccess.Entitlement) url.Values {
	q := url.Values{"change": {"1"}, "games": {strings.Join(e.Games, ",")}}
	scope := e.StoreScope
	if scope != apiaccess.ScopeAll && scope != apiaccess.ScopeBase {
		have := strings.Split(scope, ",")
		var keys []string
		for _, st := range cat.Stores {
			if st.Implied {
				continue
			}
			for _, sh := range st.Shorthands {
				if slices.Contains(have, sh) {
					keys = append(keys, st.Key)
					break
				}
			}
		}
		q.Set("stores", strings.Join(keys, ","))
		scope = apiproductlist.StoreScopeExplicit
	}
	for _, p := range cat.Packages {
		if p.StoreScope == scope {
			q.Set("package", p.Key)
			break
		}
	}
	return q
}

// storeNames maps backend shorthands to catalog store names, keeping unknown ones as is.
func storeNames(cat *apiproductlist.ProductList, shorthands []string) string {
	var names []string
	for _, sh := range shorthands {
		name := sh
		for _, st := range cat.Stores {
			if slices.Contains(st.Shorthands, sh) {
				name = st.Name
				break
			}
		}
		if name != "" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return strings.Join(names, ", ")
}
