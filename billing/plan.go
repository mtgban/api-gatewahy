// Package billing turns catalog plans into Stripe subscriptions and Stripe
// subscriptions into entitlement rows.
package billing

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/mtgban/api-gatewahy/apiaccess"
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
	// ModeAddons are the keys of the add-ons bought once, each granting a mode.
	ModeAddons []string
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

// ValidationError is a plan the catalog rejects; the message is safe to show a customer.
type ValidationError struct {
	Msg string
}

func (e *ValidationError) Error() string { return "billing: " + e.Msg }

// IsValidation reports whether err is a ValidationError.
func IsValidation(err error) bool {
	var ve *ValidationError
	return errors.As(err, &ve)
}

func invalid(format string, args ...any) error {
	return &ValidationError{Msg: fmt.Sprintf(format, args...)}
}

// Normalize canonicalizes the plan against the catalog: package and interval
// exist, at least one game is named, games are lowercase and sorted, stores
// are lowercase family keys, sorted, and only on an explicit package, mode
// add-ons are lowercase, sorted, and each one a catalog add-on granting a
// mode the gateway serves, and every add-on applies to the package. Resolve
// checks the keys exist.
func (p Plan) Normalize(cat *apiproductlist.ProductList) (Plan, error) {
	pkg, ok := cat.Package(p.Package)
	if !ok {
		return Plan{}, invalid("unknown package %q", p.Package)
	}
	if _, ok := cat.Interval(p.Interval); !ok {
		return Plan{}, invalid("unknown interval %q", p.Interval)
	}
	games := dedupe(p.Games, strings.ToLower)
	if len(games) == 0 {
		return Plan{}, invalid("pick at least one game")
	}
	slices.Sort(games)
	stores := dedupe(p.Stores, strings.ToLower)
	slices.Sort(stores)
	modeAddons := dedupe(p.ModeAddons, strings.ToLower)
	slices.Sort(modeAddons)
	out := Plan{Package: pkg.Key, Interval: p.Interval, Games: games, Stores: stores, ModeAddons: modeAddons}
	if pkg.StoreScope == apiproductlist.StoreScopeExplicit {
		if len(stores) == 0 {
			return Plan{}, invalid("pick at least one store")
		}
	} else if len(stores) > 0 {
		return Plan{}, invalid("package %s does not take a store list", pkg.Key)
	}
	for _, k := range modeAddons {
		addon, ok := cat.Addon(k)
		switch {
		case !ok:
			return Plan{}, invalid("unknown add-on %q", k)
		case addon.GrantsMode == "":
			return Plan{}, invalid("add-on %s cannot be picked on its own", k)
		case !slices.Contains(apiaccess.ValidModes, addon.GrantsMode):
			return Plan{}, invalid("add-on %s grants mode %s, which the gateway does not serve", k, addon.GrantsMode)
		case !addon.Applies(pkg.Key):
			return Plan{}, invalid("package %s cannot add %s", pkg.Key, k)
		}
	}
	for _, li := range out.LineItems(cat) {
		if li.Key == pkg.Key {
			continue
		}
		addon, ok := cat.Addon(li.Key)
		if !ok || !addon.Applies(pkg.Key) {
			return Plan{}, invalid("package %s cannot add %s", pkg.Key, li.Key)
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
			return Plan{}, invalid("unknown game %q", g)
		}
	}
	if iv, _ := cat.Interval(p.Interval); !iv.Public && !haveInvite {
		return Plan{}, ErrInviteRequired
	}
	return p, nil
}

// LineItems derives what a normalized plan bills: the package, extra stores
// past the included count, games past the included count, and each mode
// add-on once.
func (p Plan) LineItems(cat *apiproductlist.ProductList) []LineItem {
	pkg, _ := cat.Package(p.Package)
	items := []LineItem{{Key: pkg.Key, LookupKey: apiproductlist.LookupKey(pkg.Key, p.Interval), Quantity: 1, Monthly: pkg.Monthly}}
	if pkg.StoreScope == apiproductlist.StoreScopeExplicit {
		if n := int64(len(p.Stores) - pkg.IncludedStores); n > 0 {
			items = append(items, p.addonItem(cat, AddonExtraStore, n))
		}
	}
	if n := int64(len(p.Games) - cat.IncludedGames); n > 0 {
		items = append(items, p.addonItem(cat, AddonExtraGame, n))
	}
	for _, k := range p.ModeAddons {
		items = append(items, p.addonItem(cat, k, 1))
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
		"addons":     strings.Join(p.ModeAddons, ","),
		"account_id": strconv.FormatInt(accountID, 10),
	}
}

// ErrNoPlan means the subscription metadata carries no plan.
var ErrNoPlan = errors.New("billing: subscription metadata carries no plan")

// PlanFromMetadata reads back what Metadata wrote. It does not normalize.
// Metadata without addons, written before mode add-ons, reads as none.
func PlanFromMetadata(m map[string]string) (Plan, int64, error) {
	if m["package"] == "" || m["interval"] == "" {
		return Plan{}, 0, ErrNoPlan
	}
	accountID, _ := strconv.ParseInt(m["account_id"], 10, 64)
	return Plan{
		Package:    m["package"],
		Interval:   m["interval"],
		Games:      apiaccess.SplitList(m["games"]),
		Stores:     apiaccess.SplitList(m["stores"]),
		ModeAddons: apiaccess.SplitList(m["addons"]),
	}, accountID, nil
}

// equal reports whether p and q name the same package, interval, games, stores, and mode add-ons.
func (p Plan) equal(q Plan) bool {
	return p.Package == q.Package && p.Interval == q.Interval && slices.Equal(p.Games, q.Games) && slices.Equal(p.Stores, q.Stores) &&
		slices.Equal(p.ModeAddons, q.ModeAddons)
}

// planModes is the package's modes plus each mode add-on's, in apiaccess.ValidModes order.
func planModes(cat *apiproductlist.ProductList, pkg apiproductlist.Package, modeAddons []string) []string {
	granted := slices.Clone(pkg.Modes)
	for _, k := range modeAddons {
		if addon, ok := cat.Addon(k); ok && addon.GrantsMode != "" {
			granted = append(granted, addon.GrantsMode)
		}
	}
	var out []string
	for _, m := range apiaccess.ValidModes {
		if slices.Contains(granted, m) {
			out = append(out, m)
		}
	}
	return out
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
		fmt.Fprintf(&b, ", stores %s", strings.Join(p.Stores, ","))
	}
	for _, k := range p.ModeAddons {
		if addon, ok := cat.Addon(k); ok {
			fmt.Fprintf(&b, ", with %s", strings.ToLower(addon.Name))
		}
	}
	fmt.Fprintf(&b, ", %s", p.Interval)
	if total, err := p.Total(cat); err == nil {
		fmt.Fprintf(&b, ", %s per period", Dollars(total))
	}
	return b.String()
}

// Dollars formats cents as whole dollars ($200) or with cents ($12.50),
// thousands-separated like the website's formatUSD.
func Dollars(cents int64) string {
	dollars := strconv.FormatInt(cents/100, 10)
	for i := len(dollars) - 3; i > 0; i -= 3 {
		dollars = dollars[:i] + "," + dollars[i:]
	}
	if cents%100 == 0 {
		return "$" + dollars
	}
	return "$" + dollars + "." + strconv.FormatInt(100+cents%100, 10)[1:]
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
