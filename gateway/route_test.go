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
		{"/v1/magic/retail.json", Route{"magic", "retail", "mtgban/retail.json"}, []string{"retail"}, "/api/mtgban/retail.json", false},
		{"/v1/magic/retail/NEO.csv", Route{"magic", "retail", "mtgban/retail/NEO.csv"}, []string{"retail"}, "/api/mtgban/retail/NEO.csv", false},
		{"/v1/pokemon/buylist.json", Route{"pokemon", "buylist", "mtgban/buylist.json"}, []string{"buylist"}, "/api/mtgban/buylist.json", false},
		{"/v1/magic/all.json", Route{"magic", "all", "mtgban/all.json"}, []string{"retail", "buylist"}, "/api/mtgban/all.json", false},
		{"/v1/magic/sealed/abc.json", Route{"magic", "sealed", "mtgban/sealed/abc.json"}, []string{"sealed"}, "/api/mtgban/sealed/abc.json", false},
		{"/v1/magic/sets.json", Route{"magic", "meta", "mtgban/sets.json"}, nil, "/api/mtgban/sets.json", false},
		{"/v1/magic/stores.csv", Route{"magic", "meta", "mtgban/stores.csv"}, nil, "/api/mtgban/stores.csv", false},
		{"/v1/magic/search/x.json", Route{"magic", "search", "mtgban/search/x.json"}, nil, "/api/mtgban/search/x.json", false},
		{"/v1/magic/retailer.json", Route{}, nil, "", true},
		{"/v1/magic/other/retail.json", Route{}, nil, "", true},
		{"/v1/magic/mtgban/retail.json", Route{}, nil, "", true},
		{"/v1/magic/", Route{}, nil, "", true},
		{"/v1/Magic/mtgban/retail.json", Route{}, nil, "", true},
		{"/v2/magic/retail.json", Route{"magic", "retail", "v2/retail.json"}, []string{"retail"}, "/api/v2/retail.json", false},
		{"/v2/pokemon/all/SV1.json", Route{"pokemon", "all", "v2/all/SV1.json"}, []string{"retail", "buylist"}, "/api/v2/all/SV1.json", false},
		{"/v2/magic/sealed.json", Route{"magic", "sealed", "v2/sealed.json"}, []string{"sealed"}, "/api/v2/sealed.json", false},
		{"/v2/magic/stores.json", Route{"magic", "meta", "v2/stores.json"}, nil, "/api/v2/stores.json", false},
		{"/v2/magic/finishes.json", Route{"magic", "meta", "v2/finishes.json"}, nil, "/api/v2/finishes.json", false},
		{"/v2/magic/finishes.csv", Route{"magic", "meta", "v2/finishes.csv"}, nil, "/api/v2/finishes.csv", false},
		{"/v1/magic/finishes.json", Route{}, nil, "", true},
		{"/v2/magic/search/x.json", Route{}, nil, "", true},
		{"/v2/magic/mtgban/retail.json", Route{}, nil, "", true},
		{"/v2/magic/retail/../../secrets.json", Route{}, nil, "", true},
		{"/v3/magic/mtgban/retail.json", Route{}, nil, "", true},
		{"/api/mtgban/retail.json", Route{}, nil, "", true},
		{"/v1/magic/retail/../../secrets.json", Route{}, nil, "", true},
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
