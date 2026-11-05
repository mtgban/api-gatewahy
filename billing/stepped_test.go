package billing_test

import (
	"github.com/mtgban/mtgban-website/apiproductlist"
)

// steppedCatalog fixes the extra store and game steps at 15000/5000 and 25000/10000, whatever the shipped catalog says.
var steppedCatalog = stepped(testCatalog, map[string][2]int64{"extra_store": {15000, 5000}, "extra_game": {25000, 10000}})

// flatCatalog is testCatalog with every extra store and game at one flat amount, as before the step.
var flatCatalog = stepped(testCatalog, map[string][2]int64{"extra_store": {0, 15000}, "extra_game": {0, 15000}})

// stepped clones cat with each named add-on's first and repeat monthly amounts set.
func stepped(cat *apiproductlist.ProductList, steps map[string][2]int64) *apiproductlist.ProductList {
	out := *cat
	out.Addons = append([]apiproductlist.Addon(nil), cat.Addons...)
	for i, a := range out.Addons {
		if s, ok := steps[a.Key]; ok {
			out.Addons[i].FirstMonthly, out.Addons[i].Monthly = s[0], s[1]
		}
	}
	return &out
}

// packageMonthly reads a package's monthly amount from testCatalog.
func packageMonthly(key string) int64 {
	pkg, _ := testCatalog.Package(key)
	return pkg.Monthly
}
