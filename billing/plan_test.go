package billing

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/mtgban/mtgban-website/apiproductlist"
)

var (
	testCatalog = apiproductlist.MustLoad()
	testGames   = []string{"lorcana", "magic", "pokemon"}
)

func TestNormalizeCanonicalizes(t *testing.T) {
	p, err := Plan{Package: "starter", Interval: "monthly", Games: []string{"Pokemon", "magic", "pokemon"}, Stores: []string{"scg", "CK", "ck"}}.Normalize(testCatalog)
	if err != nil {
		t.Fatal(err)
	}
	want := Plan{Package: "starter", Interval: "monthly", Games: []string{"magic", "pokemon"}, Stores: []string{"CK", "SCG"}}
	if !reflect.DeepEqual(p, want) {
		t.Errorf("got %+v want %+v", p, want)
	}
	p, err = Plan{Package: "all_data", Interval: "quarterly"}.Normalize(testCatalog)
	if err != nil || !reflect.DeepEqual(p.Games, []string{"magic"}) || len(p.Stores) != 0 {
		t.Errorf("included game not added: %+v %v", p, err)
	}
}

func TestNormalizeRejects(t *testing.T) {
	cases := []struct {
		name string
		plan Plan
		want string
	}{
		{"unknown package", Plan{Package: "gold", Interval: "monthly"}, "package"},
		{"unknown interval", Plan{Package: "all_data", Interval: "weekly"}, "interval"},
		{"starter without stores", Plan{Package: "starter", Interval: "monthly"}, "store"},
		{"starter with TCG", Plan{Package: "starter", Interval: "monthly", Stores: []string{"TCG"}}, "not selectable"},
		{"starter unknown store", Plan{Package: "starter", Interval: "monthly", Stores: []string{"XYZ"}}, "not selectable"},
		{"preset with stores", Plan{Package: "all_stores", Interval: "monthly", Stores: []string{"CK"}}, "store list"},
	}
	for _, c := range cases {
		_, err := c.plan.Normalize(testCatalog)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want containing %q", c.name, err, c.want)
		}
	}
}

func TestValidate(t *testing.T) {
	if _, err := (Plan{Package: "all_data", Interval: "monthly", Games: []string{"yugioh"}}).Validate(testCatalog, testGames, false); err == nil || !strings.Contains(err.Error(), "game") {
		t.Errorf("unknown game: %v", err)
	}
	if _, err := (Plan{Package: "all_data", Interval: "quarterly"}).Validate(testCatalog, testGames, false); !errors.Is(err, ErrInviteRequired) {
		t.Errorf("quarterly without invite: %v", err)
	}
	if _, err := (Plan{Package: "all_data", Interval: "quarterly"}).Validate(testCatalog, testGames, true); err != nil {
		t.Errorf("quarterly with invite: %v", err)
	}
	if _, err := (Plan{Package: "all_data", Interval: "monthly", Games: []string{"pokemon"}}).Validate(testCatalog, testGames, false); err != nil {
		t.Errorf("good plan: %v", err)
	}
}

func TestLineItemsAndTotal(t *testing.T) {
	cases := []struct {
		name  string
		plan  Plan
		items []LineItem
		total int64
	}{
		{
			"starter one store",
			Plan{Package: "starter", Interval: "monthly", Stores: []string{"CK"}},
			[]LineItem{{"starter", "starter_monthly", 1, 20000}},
			20000,
		},
		{
			"starter three stores two games",
			Plan{Package: "starter", Interval: "monthly", Games: []string{"pokemon"}, Stores: []string{"CK", "SCG", "CSI"}},
			[]LineItem{{"starter", "starter_monthly", 1, 20000}, {"extra_store", "extra_store_monthly", 2, 15000}, {"extra_game", "extra_game_monthly", 1, 15000}},
			65000,
		},
		{
			"all stores",
			Plan{Package: "all_stores", Interval: "monthly"},
			[]LineItem{{"all_stores", "all_stores_monthly", 1, 50000}},
			50000,
		},
		{
			"all data quarterly two extra games",
			Plan{Package: "all_data", Interval: "quarterly", Games: []string{"pokemon", "lorcana"}},
			[]LineItem{{"all_data", "all_data_quarterly", 1, 80000}, {"extra_game", "extra_game_quarterly", 2, 15000}},
			330000,
		},
	}
	for _, c := range cases {
		p, err := c.plan.Normalize(testCatalog)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := p.LineItems(testCatalog); !reflect.DeepEqual(got, c.items) {
			t.Errorf("%s: items %+v want %+v", c.name, got, c.items)
		}
		if got, err := p.Total(testCatalog); err != nil || got != c.total {
			t.Errorf("%s: total %d %v want %d", c.name, got, err, c.total)
		}
	}
}

func TestMetadataRoundTrip(t *testing.T) {
	p, _ := Plan{Package: "starter", Interval: "quarterly", Games: []string{"pokemon"}, Stores: []string{"CK", "SCG"}}.Normalize(testCatalog)
	m := p.Metadata(42)
	want := map[string]string{"package": "starter", "interval": "quarterly", "games": "magic,pokemon", "stores": "CK,SCG", "account_id": "42"}
	if !reflect.DeepEqual(m, want) {
		t.Errorf("metadata %v", m)
	}
	back, accountID, err := PlanFromMetadata(m)
	if err != nil || accountID != 42 || !reflect.DeepEqual(back, p) {
		t.Errorf("round trip %+v %d %v", back, accountID, err)
	}
	empty, _ := Plan{Package: "all_data", Interval: "monthly"}.Normalize(testCatalog)
	back, _, err = PlanFromMetadata(empty.Metadata(1))
	if err != nil || len(back.Stores) != 0 {
		t.Errorf("empty stores round trip: %+v %v", back, err)
	}
	if _, _, err := PlanFromMetadata(map[string]string{}); err == nil {
		t.Error("empty metadata accepted")
	}
	if _, id, _ := PlanFromMetadata(map[string]string{"package": "x", "interval": "y"}); id != 0 {
		t.Errorf("missing account_id gave %d", id)
	}
}

func TestEntitlementAndAddons(t *testing.T) {
	starter, _ := Plan{Package: "starter", Interval: "monthly", Games: []string{"pokemon"}, Stores: []string{"SCG", "CK"}}.Normalize(testCatalog)
	scope, modes := starter.Entitlement(testCatalog)
	if scope != "TCGLow,TCGMarket,TCGDirect,TCGDirectNet,TCGPlayer,CK,SCG" || !reflect.DeepEqual(modes, []string{"retail", "buylist"}) {
		t.Errorf("starter entitlement %q %v", scope, modes)
	}
	if keys := starter.StoreKeys(testCatalog); !reflect.DeepEqual(keys, []string{"TCG", "CK", "SCG"}) {
		t.Errorf("starter store keys %v", keys)
	}
	if got := starter.Addons(testCatalog); !reflect.DeepEqual(got, []string{"extra_store:1", "extra_game:1"}) {
		t.Errorf("starter addons %v", got)
	}
	allData, _ := Plan{Package: "all_data", Interval: "monthly"}.Normalize(testCatalog)
	scope, modes = allData.Entitlement(testCatalog)
	if scope != "ALL_ACCESS" || len(modes) != 3 {
		t.Errorf("all_data entitlement %q %v", scope, modes)
	}
	if got := allData.Addons(testCatalog); len(got) != 0 {
		t.Errorf("all_data addons %v", got)
	}
}

func TestNormalizeErrorsAreValidationErrors(t *testing.T) {
	bad := []Plan{
		{Package: "nope", Interval: "monthly", Games: []string{"magic"}},
		{Package: "starter", Interval: "weekly", Games: []string{"magic"}},
		{Package: "starter", Interval: "monthly", Games: []string{"magic"}},
		{Package: "starter", Interval: "monthly", Games: []string{"magic"}, Stores: []string{"TCG"}},
		{Package: "all_data", Interval: "monthly", Games: []string{"magic"}, Stores: []string{"CK"}},
	}
	for _, p := range bad {
		if _, err := p.Normalize(testCatalog); !IsValidation(err) {
			t.Errorf("%+v: %v is not a ValidationError", p, err)
		}
	}
	if _, err := (Plan{Package: "all_data", Interval: "monthly", Games: []string{"chess"}}).Validate(testCatalog, []string{"magic"}, true); !IsValidation(err) {
		t.Errorf("unknown game: %v", err)
	}
	if _, err := (Plan{Package: "all_data", Interval: "quarterly", Games: []string{"magic"}}).Validate(testCatalog, []string{"magic"}, false); !errors.Is(err, ErrInviteRequired) || IsValidation(err) {
		t.Errorf("invite: %v", err)
	}
	if IsValidation(errors.New("billing: other")) {
		t.Error("plain error classified as validation")
	}
}

func TestDescribeAndDollars(t *testing.T) {
	p, _ := Plan{Package: "starter", Interval: "quarterly", Games: []string{"pokemon"}, Stores: []string{"CK"}}.Normalize(testCatalog)
	got := p.Describe(testCatalog)
	for _, want := range []string{"TCGplayer plus one store", "magic,pokemon", "TCG,CK", "quarterly", "$1,050"} {
		if !strings.Contains(got, want) {
			t.Errorf("describe %q lacks %q", got, want)
		}
	}
	if Dollars(5) != "$0.05" || Dollars(123456) != "$1,234.56" || Dollars(20000) != "$200" {
		t.Errorf("dollars %q %q %q", Dollars(5), Dollars(123456), Dollars(20000))
	}
}
