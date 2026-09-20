// Package billing turns catalog plans into Stripe subscriptions and Stripe
// subscriptions into entitlement rows.
package billing

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/mtgban/mtgban-website/apiproductlist"
)

// Add-on keys the plan model knows how to count.
const (
	AddonExtraStore = "extra_store"
	AddonExtraGame  = "extra_game"
)

// Plan is what a customer chose: the input to checkout and what metadata preserves.
type Plan struct {
	Package  string
	Interval string
	Games    []string
	Stores   []string
}

// LineItem is one Stripe price and quantity a plan bills.
type LineItem struct {
	Key       string
	LookupKey string
	Quantity  int64
	Monthly   int64
}

// ErrInviteRequired means the plan names an interval only an invite unlocks.
var ErrInviteRequired = errors.New("billing: this interval requires an invite")

// Normalize canonicalizes the plan against the catalog: package and interval
// exist, games are lowercase, sorted, and include the catalog's included
// games, stores are uppercase, sorted, selectable, and only on an explicit
// package, and every implied add-on applies to the package.
func (p Plan) Normalize(cat *apiproductlist.ProductList) (Plan, error) {
	pkg, ok := cat.Package(p.Package)
	if !ok {
		return Plan{}, fmt.Errorf("billing: unknown package %q", p.Package)
	}
	if _, ok := cat.Interval(p.Interval); !ok {
		return Plan{}, fmt.Errorf("billing: unknown interval %q", p.Interval)
	}
	games := dedupe(p.Games, strings.ToLower)
	for _, g := range cat.IncludedGames {
		if !slices.Contains(games, g) {
			games = append(games, g)
		}
	}
	slices.Sort(games)
	stores := dedupe(p.Stores, strings.ToUpper)
	slices.Sort(stores)
	out := Plan{Package: pkg.Key, Interval: p.Interval, Games: games, Stores: stores}
	if pkg.StoreScope == apiproductlist.StoreScopeExplicit {
		if len(stores) == 0 {
			return Plan{}, errors.New("billing: pick at least one store")
		}
		for _, s := range stores {
			if st, ok := cat.Store(s); !ok || st.Implied {
				return Plan{}, fmt.Errorf("billing: store %q is not selectable", s)
			}
		}
	} else if len(stores) > 0 {
		return Plan{}, fmt.Errorf("billing: package %s does not take a store list", pkg.Key)
	}
	for _, li := range out.LineItems(cat) {
		if li.Key == pkg.Key {
			continue
		}
		addon, ok := cat.Addon(li.Key)
		if !ok || !addon.Applies(pkg.Key) {
			return Plan{}, fmt.Errorf("billing: package %s cannot add %s", pkg.Key, li.Key)
		}
	}
	return out, nil
}

// Validate is Normalize plus the checks a new checkout needs.
func (p Plan) Validate(cat *apiproductlist.ProductList, knownGames []string, haveInvite bool) (Plan, error) {
	p, err := p.Normalize(cat)
	if err != nil {
		return Plan{}, err
	}
	for _, g := range p.Games {
		if !slices.Contains(knownGames, g) {
			return Plan{}, fmt.Errorf("billing: unknown game %q", g)
		}
	}
	if iv, _ := cat.Interval(p.Interval); !iv.Public && !haveInvite {
		return Plan{}, ErrInviteRequired
	}
	return p, nil
}

// LineItems derives what a normalized plan bills: the package, extra stores
// past the included count, and games past the included ones.
func (p Plan) LineItems(cat *apiproductlist.ProductList) []LineItem {
	pkg, _ := cat.Package(p.Package)
	items := []LineItem{{Key: pkg.Key, LookupKey: apiproductlist.LookupKey(pkg.Key, p.Interval), Quantity: 1, Monthly: pkg.Monthly}}
	if pkg.StoreScope == apiproductlist.StoreScopeExplicit {
		if n := int64(len(p.Stores) - pkg.IncludedStores); n > 0 {
			items = append(items, p.addonItem(cat, AddonExtraStore, n))
		}
	}
	if n := int64(len(p.Games) - len(cat.IncludedGames)); n > 0 {
		items = append(items, p.addonItem(cat, AddonExtraGame, n))
	}
	return items
}

func (p Plan) addonItem(cat *apiproductlist.ProductList, key string, qty int64) LineItem {
	addon, _ := cat.Addon(key)
	return LineItem{Key: key, LookupKey: apiproductlist.LookupKey(key, p.Interval), Quantity: qty, Monthly: addon.Monthly}
}

// Total is the amount billed per period, in cents.
func (p Plan) Total(cat *apiproductlist.ProductList) (int64, error) {
	iv, _ := cat.Interval(p.Interval)
	var total int64
	for _, li := range p.LineItems(cat) {
		amount, err := iv.Amount(li.Monthly)
		if err != nil {
			return 0, err
		}
		total += amount * li.Quantity
	}
	return total, nil
}

// Metadata is what checkout writes on the subscription so reconcile can rebuild the plan.
func (p Plan) Metadata(accountID int64) map[string]string {
	return map[string]string{
		"package":    p.Package,
		"interval":   p.Interval,
		"games":      strings.Join(p.Games, ","),
		"stores":     strings.Join(p.Stores, ","),
		"account_id": strconv.FormatInt(accountID, 10),
	}
}

// PlanFromMetadata reads back what Metadata wrote. It does not normalize.
func PlanFromMetadata(m map[string]string) (Plan, int64, error) {
	if m["package"] == "" || m["interval"] == "" {
		return Plan{}, 0, errors.New("billing: subscription metadata carries no plan")
	}
	accountID, _ := strconv.ParseInt(m["account_id"], 10, 64)
	return Plan{
		Package:  m["package"],
		Interval: m["interval"],
		Games:    splitList(m["games"]),
		Stores:   splitList(m["stores"]),
	}, accountID, nil
}

// Entitlement is the store scope and modes the plan grants. An explicit
// scope is the backend shorthands of the implied stores and the picked ones.
func (p Plan) Entitlement(cat *apiproductlist.ProductList) (string, []string) {
	pkg, _ := cat.Package(p.Package)
	if pkg.StoreScope != apiproductlist.StoreScopeExplicit {
		return pkg.StoreScope, pkg.Modes
	}
	var shorthands []string
	for _, s := range cat.ImpliedStores() {
		shorthands = append(shorthands, s.Shorthands...)
	}
	for _, key := range p.Stores {
		s, _ := cat.Store(key)
		shorthands = append(shorthands, s.Shorthands...)
	}
	return strings.Join(shorthands, ","), pkg.Modes
}

// StoreKeys lists the implied and picked store keys, for people rather than the backend.
func (p Plan) StoreKeys(cat *apiproductlist.ProductList) []string {
	var keys []string
	for _, s := range cat.ImpliedStores() {
		keys = append(keys, s.Key)
	}
	return append(keys, p.Stores...)
}

// Addons lists the add-ons with quantities for the entitlement's addons column.
func (p Plan) Addons(cat *apiproductlist.ProductList) []string {
	out := []string{}
	for _, li := range p.LineItems(cat) {
		if li.Key != p.Package {
			out = append(out, fmt.Sprintf("%s:%d", li.Key, li.Quantity))
		}
	}
	return out
}

// Describe renders the plan in words for operators and confirmation pages.
func (p Plan) Describe(cat *apiproductlist.ProductList) string {
	pkg, _ := cat.Package(p.Package)
	var b strings.Builder
	fmt.Fprintf(&b, "%s, games %s", pkg.Name, strings.Join(p.Games, ","))
	if pkg.StoreScope == apiproductlist.StoreScopeExplicit {
		fmt.Fprintf(&b, ", stores %s", strings.Join(p.StoreKeys(cat), ","))
	}
	fmt.Fprintf(&b, ", %s", p.Interval)
	if total, err := p.Total(cat); err == nil {
		fmt.Fprintf(&b, ", %s per period", Dollars(total))
	}
	return b.String()
}

// Dollars formats cents as $12.34.
func Dollars(cents int64) string {
	return fmt.Sprintf("$%d.%02d", cents/100, cents%100)
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func dedupe(in []string, canon func(string) string) []string {
	var out []string
	for _, s := range in {
		s = canon(strings.TrimSpace(s))
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}
