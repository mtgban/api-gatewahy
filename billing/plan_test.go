package billing_test

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/mtgban/api-gatewahy/billing"
	"github.com/mtgban/mtgban-website/apiproductlist"
)

var (
	testCatalog = apiproductlist.MustLoad()
	testGames   = []string{"lorcana", "magic", "pokemon"}
)

func TestNormalizeCanonicalizes(t *testing.T) {
	p, err := billing.Plan{Package: "starter", Interval: "monthly", Games: []string{"Pokemon", "magic", "pokemon"}, Stores: []string{"starcitygames", "CardKingdom", "cardkingdom"}}.Normalize(testCatalog)
	if err != nil {
		t.Fatal(err)
	}
	want := billing.Plan{Package: "starter", Interval: "monthly", Games: []string{"magic", "pokemon"}, Stores: []string{"cardkingdom", "starcitygames"}}
	if !planEqual(p, want) {
		t.Errorf("got %+v want %+v", p, want)
	}
	p, err = billing.Plan{Package: "all_data", Interval: "quarterly", Games: []string{"Pokemon"}}.Normalize(testCatalog)
	if err != nil || !slices.Equal(p.Games, []string{"pokemon"}) || len(p.Stores) != 0 {
		t.Errorf("any game can be the included one: %+v %v", p, err)
	}
	if _, err := (billing.Plan{Package: "all_data", Interval: "quarterly"}).Normalize(testCatalog); !billing.IsValidation(err) {
		t.Errorf("no game should be a validation error, got %v", err)
	}
}

func TestNormalizeRejects(t *testing.T) {
	cases := []struct {
		name string
		plan billing.Plan
		want string
	}{
		{"unknown package", billing.Plan{Package: "gold", Interval: "monthly", Games: []string{"magic"}}, "package"},
		{"unknown interval", billing.Plan{Package: "all_data", Interval: "weekly", Games: []string{"magic"}}, "interval"},
		{"starter without stores", billing.Plan{Package: "starter", Interval: "monthly", Games: []string{"magic"}}, "store"},
		{"preset with stores", billing.Plan{Package: "all_stores", Interval: "monthly", Games: []string{"magic"}, Stores: []string{"cardkingdom"}}, "store list"},
	}
	for _, c := range cases {
		_, err := c.plan.Normalize(testCatalog)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want containing %q", c.name, err, c.want)
		}
	}
}

func TestValidate(t *testing.T) {
	if _, err := (billing.Plan{Package: "all_data", Interval: "monthly", Games: []string{"yugioh"}}).Validate(testCatalog, testGames, false); err == nil || !strings.Contains(err.Error(), "game") {
		t.Errorf("unknown game: %v", err)
	}
	if _, err := (billing.Plan{Package: "all_data", Interval: "quarterly", Games: []string{"magic"}}).Validate(testCatalog, testGames, false); !errors.Is(err, billing.ErrInviteRequired) {
		t.Errorf("quarterly without invite: %v", err)
	}
	if _, err := (billing.Plan{Package: "all_data", Interval: "quarterly", Games: []string{"magic"}}).Validate(testCatalog, testGames, true); err != nil {
		t.Errorf("quarterly with invite: %v", err)
	}
	if _, err := (billing.Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic", "pokemon"}}).Validate(testCatalog, testGames, false); err != nil {
		t.Errorf("good plan: %v", err)
	}
}

func TestLineItemsAndTotal(t *testing.T) {
	cases := []struct {
		name  string
		plan  billing.Plan
		items []billing.LineItem
		total int64
	}{
		{
			"starter one store",
			billing.Plan{Package: "starter", Interval: "monthly", Games: []string{"magic"}, Stores: []string{"cardkingdom"}},
			[]billing.LineItem{{"starter", "starter_monthly", 1, 20000}},
			20000,
		},
		{
			"starter three stores two games",
			billing.Plan{Package: "starter", Interval: "monthly", Games: []string{"magic", "pokemon"}, Stores: []string{"cardkingdom", "starcitygames", "coolstuffinc"}},
			[]billing.LineItem{{"starter", "starter_monthly", 1, 20000}, {"extra_store", "extra_store_monthly", 2, 15000}, {"extra_game", "extra_game_monthly", 1, 15000}},
			65000,
		},
		{
			"all stores",
			billing.Plan{Package: "all_stores", Interval: "monthly", Games: []string{"magic"}},
			[]billing.LineItem{{"all_stores", "all_stores_monthly", 1, 50000}},
			50000,
		},
		{
			"all data quarterly two extra games",
			billing.Plan{Package: "all_data", Interval: "quarterly", Games: []string{"magic", "pokemon", "lorcana"}},
			[]billing.LineItem{{"all_data", "all_data_quarterly", 1, 80000}, {"extra_game", "extra_game_quarterly", 2, 15000}},
			330000,
		},
	}
	for _, c := range cases {
		p, err := c.plan.Normalize(testCatalog)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := p.LineItems(testCatalog); !slices.Equal(got, c.items) {
			t.Errorf("%s: items %+v want %+v", c.name, got, c.items)
		}
		if got, err := p.Total(testCatalog); err != nil || got != c.total {
			t.Errorf("%s: total %d %v want %d", c.name, got, err, c.total)
		}
	}
}

func TestMetadataRoundTrip(t *testing.T) {
	p, _ := billing.Plan{Package: "starter", Interval: "quarterly", Games: []string{"magic", "pokemon"}, Stores: []string{"cardkingdom", "starcitygames"}}.Normalize(testCatalog)
	m := p.Metadata(42)
	want := map[string]string{"package": "starter", "interval": "quarterly", "games": "magic,pokemon", "stores": "cardkingdom,starcitygames", "account_id": "42"}
	if !maps.Equal(m, want) {
		t.Errorf("metadata %v", m)
	}
	back, accountID, err := billing.PlanFromMetadata(m)
	if err != nil || accountID != 42 || !planEqual(back, p) {
		t.Errorf("round trip %+v %d %v", back, accountID, err)
	}
	empty, _ := billing.Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}.Normalize(testCatalog)
	back, _, err = billing.PlanFromMetadata(empty.Metadata(1))
	if err != nil || len(back.Stores) != 0 {
		t.Errorf("empty stores round trip: %+v %v", back, err)
	}
	if _, _, err := billing.PlanFromMetadata(map[string]string{}); err == nil {
		t.Error("empty metadata accepted")
	}
	if _, id, _ := billing.PlanFromMetadata(map[string]string{"package": "x", "interval": "y"}); id != 0 {
		t.Errorf("missing account_id gave %d", id)
	}
}

func TestAddons(t *testing.T) {
	starter, _ := billing.Plan{Package: "starter", Interval: "monthly", Games: []string{"magic", "pokemon"}, Stores: []string{"starcitygames", "cardkingdom"}}.Normalize(testCatalog)
	if got := starter.Addons(testCatalog); !slices.Equal(got, []string{"extra_store:1", "extra_game:1"}) {
		t.Errorf("starter addons %v", got)
	}
	allData, _ := billing.Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}.Normalize(testCatalog)
	if got := allData.Addons(testCatalog); len(got) != 0 {
		t.Errorf("all_data addons %v", got)
	}
}

func TestNormalizeErrorsAreValidationErrors(t *testing.T) {
	bad := []billing.Plan{
		{Package: "nope", Interval: "monthly", Games: []string{"magic"}},
		{Package: "starter", Interval: "weekly", Games: []string{"magic"}},
		{Package: "starter", Interval: "monthly", Games: []string{"magic"}},
		{Package: "all_data", Interval: "monthly", Games: []string{"magic"}, Stores: []string{"cardkingdom"}},
	}
	for _, p := range bad {
		if _, err := p.Normalize(testCatalog); !billing.IsValidation(err) {
			t.Errorf("%+v: %v is not a ValidationError", p, err)
		}
	}
	if _, err := (billing.Plan{Package: "all_data", Interval: "monthly", Games: []string{"chess"}}).Validate(testCatalog, []string{"magic"}, true); !billing.IsValidation(err) {
		t.Errorf("unknown game: %v", err)
	}
	if _, err := (billing.Plan{Package: "all_data", Interval: "quarterly", Games: []string{"magic"}}).Validate(testCatalog, []string{"magic"}, false); !errors.Is(err, billing.ErrInviteRequired) || billing.IsValidation(err) {
		t.Errorf("invite: %v", err)
	}
	if billing.IsValidation(errors.New("billing: other")) {
		t.Error("plain error classified as validation")
	}
}

func TestDescribeAndDollars(t *testing.T) {
	p, _ := billing.Plan{Package: "starter", Interval: "quarterly", Games: []string{"magic", "pokemon"}, Stores: []string{"cardkingdom"}}.Normalize(testCatalog)
	got := p.Describe(testCatalog)
	for _, want := range []string{"À la carte", "magic,pokemon", "stores cardkingdom", "quarterly", "$1,050"} {
		if !strings.Contains(got, want) {
			t.Errorf("describe %q lacks %q", got, want)
		}
	}
	if billing.Dollars(5) != "$0.05" || billing.Dollars(123456) != "$1,234.56" || billing.Dollars(20000) != "$200" {
		t.Errorf("dollars %q %q %q", billing.Dollars(5), billing.Dollars(123456), billing.Dollars(20000))
	}
}

func planEqual(a, b billing.Plan) bool {
	return a.Package == b.Package && a.Interval == b.Interval &&
		slices.Equal(a.Games, b.Games) && slices.Equal(a.Stores, b.Stores)
}
