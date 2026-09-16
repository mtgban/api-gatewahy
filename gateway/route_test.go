package gateway

import (
	"errors"
	"slices"
	"testing"
)

func TestParseRoute(t *testing.T) {
	cases := []struct {
		path      string
		want      Route
		needs     []string
		backend   string
		wantError bool
	}{
		{"/v1/magic/mtgban/retail.json", Route{"magic", "retail", "mtgban/retail.json"}, []string{"retail"}, "/api/mtgban/retail.json", false},
		{"/v1/magic/mtgban/retail/NEO.csv", Route{"magic", "retail", "mtgban/retail/NEO.csv"}, []string{"retail"}, "/api/mtgban/retail/NEO.csv", false},
		{"/v1/pokemon/mtgban/buylist.json", Route{"pokemon", "buylist", "mtgban/buylist.json"}, []string{"buylist"}, "/api/mtgban/buylist.json", false},
		{"/v1/magic/mtgban/all.json", Route{"magic", "all", "mtgban/all.json"}, []string{"retail", "buylist"}, "/api/mtgban/all.json", false},
		{"/v1/magic/mtgban/sealed/abc.json", Route{"magic", "sealed", "mtgban/sealed/abc.json"}, []string{"sealed"}, "/api/mtgban/sealed/abc.json", false},
		{"/v1/magic/mtgban/sets.json", Route{"magic", "meta", "mtgban/sets.json"}, nil, "/api/mtgban/sets.json", false},
		{"/v1/magic/mtgban/stores.csv", Route{"magic", "meta", "mtgban/stores.csv"}, nil, "/api/mtgban/stores.csv", false},
		{"/v1/magic/mtgban/search/x.json", Route{"magic", "search", "mtgban/search/x.json"}, nil, "/api/mtgban/search/x.json", false},
		{"/v1/magic/mtgban/retailer.json", Route{}, nil, "", true},
		{"/v1/magic/other/retail.json", Route{}, nil, "", true},
		{"/v1/magic/", Route{}, nil, "", true},
		{"/v1/Magic/mtgban/retail.json", Route{}, nil, "", true},
		{"/v2/magic/mtgban/retail.json", Route{}, nil, "", true},
		{"/api/mtgban/retail.json", Route{}, nil, "", true},
		{"/v1/magic/mtgban/retail/../../secrets.json", Route{}, nil, "", true},
	}
	for _, c := range cases {
		got, err := ParseRoute(c.path)
		if c.wantError {
			if !errors.Is(err, ErrNoRoute) {
				t.Errorf("%s: err %v", c.path, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: got %+v err %v", c.path, got, err)
			continue
		}
		if !slices.Equal(got.NeedsModes(), c.needs) || got.BackendPath() != c.backend {
			t.Errorf("%s: needs %v backend %q", c.path, got.NeedsModes(), got.BackendPath())
		}
	}
}
