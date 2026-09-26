package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mtgban/api-gatewahy/config"
)

func TestResolveUnionsImpliedAndSelected(t *testing.T) {
	lister := newFakeStores()
	p, err := Plan{Package: "starter", Interval: "monthly", Games: []string{"magic", "pokemon"}, Stores: []string{"starcitygames", "cardkingdom"}}.Normalize(testCatalog)
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.Resolve(context.Background(), testCatalog, lister)
	if err != nil {
		t.Fatal(err)
	}
	if r.Scope != "CK,CKBLLast,SCG,TCGDirect,TCGDirectNet,TCGLow,TCGMarket,TCGPlayer" {
		t.Errorf("scope %q", r.Scope)
	}
	if !reflect.DeepEqual(r.Modes, []string{"retail", "buylist"}) {
		t.Errorf("modes %v", r.Modes)
	}
	if got := r.StoreNames(); !reflect.DeepEqual(got, []string{"TCGplayer", "Card Kingdom", "Star City Games"}) {
		t.Errorf("names %v", got)
	}
}

func TestResolveRejectsKeysNoSiteSells(t *testing.T) {
	lister := newFakeStores()
	cases := []struct {
		games []string
		key   string
	}{
		{[]string{"magic"}, "xyz"},
		{[]string{"magic"}, "trollandtoad"},
		{[]string{"magic", "pokemon"}, "tcgplayer"},
	}
	for _, c := range cases {
		p, err := Plan{Package: "starter", Interval: "monthly", Games: c.games, Stores: []string{c.key}}.Normalize(testCatalog)
		if err != nil {
			t.Fatal(err)
		}
		_, err = p.Resolve(context.Background(), testCatalog, lister)
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Msg != "Store "+c.key+" is not available for the games you picked." {
			t.Errorf("%v %s: %v", c.games, c.key, err)
		}
	}
	p, _ := Plan{Package: "starter", Interval: "monthly", Games: []string{"magic", "pokemon"}, Stores: []string{"trollandtoad"}}.Normalize(testCatalog)
	if r, err := p.Resolve(context.Background(), testCatalog, lister); err != nil || r.Scope != "TCGDirect,TCGDirectNet,TCGLow,TCGMarket,TCGPlayer,TNT" {
		t.Errorf("key on one of two sites: %+v %v", r, err)
	}
}

func TestResolveSiteDownIsNotValidation(t *testing.T) {
	lister := newFakeStores()
	lister.fail = errors.New("connection refused")
	p, _ := Plan{Package: "starter", Interval: "monthly", Games: []string{"magic"}, Stores: []string{"cardkingdom"}}.Normalize(testCatalog)
	_, err := p.Resolve(context.Background(), testCatalog, lister)
	if !errors.Is(err, ErrStoresUnavailable) || IsValidation(err) {
		t.Errorf("site down: %v", err)
	}
}

func TestResolvePresetSkipsTheSites(t *testing.T) {
	lister := newFakeStores()
	lister.fail = errors.New("not called")
	p, _ := Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}.Normalize(testCatalog)
	r, err := p.Resolve(context.Background(), testCatalog, lister)
	if err != nil || r.Scope != "ALL_ACCESS" || len(r.Modes) != 3 || lister.calls != 0 || len(r.StoreNames()) != 0 {
		t.Errorf("preset %+v %v calls %d", r, err, lister.calls)
	}
}

// storesServer serves body for /api-plans/stores.json with status, counting hits.
func storesServer(t *testing.T, status *atomic.Int32, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/api-plans/stores.json" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestSiteStoreClientFetchesAndCaches(t *testing.T) {
	want := newFakeStores().sites["magic"]
	body, _ := json.Marshal(want)
	var status atomic.Int32
	status.Store(http.StatusOK)
	srv, hits := storesServer(t, &status, string(body))
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	c := &SiteStoreClient{Origins: map[string]string{"magic": srv.URL}, TTL: 5 * time.Minute, Now: func() time.Time { return now }}
	ctx := context.Background()

	got, err := c.SiteStores(ctx, "magic")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("first fetch %+v %v", got, err)
	}
	if _, err := c.SiteStores(ctx, "magic"); err != nil || hits.Load() != 1 {
		t.Errorf("cached fetch hit the site: %d %v", hits.Load(), err)
	}
	now = now.Add(6 * time.Minute)
	status.Store(http.StatusInternalServerError)
	got, err = c.SiteStores(ctx, "magic")
	if err != nil || !reflect.DeepEqual(got, want) || hits.Load() != 2 {
		t.Errorf("stale value after a failed refresh: %+v %v hits %d", got, err, hits.Load())
	}
	if _, err := c.SiteStores(ctx, "pokemon"); err == nil {
		t.Error("a game with no origin resolved")
	}
}

func TestSiteStoreClientErrorsWithoutCache(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusBadGateway)
	srv, _ := storesServer(t, &status, "")
	c := &SiteStoreClient{Origins: map[string]string{"magic": srv.URL}}
	if _, err := c.SiteStores(context.Background(), "magic"); err == nil {
		t.Error("a failed fetch with nothing cached succeeded")
	}
	status.Store(http.StatusOK)
	srv2, _ := storesServer(t, &status, "not json")
	c = &SiteStoreClient{Origins: map[string]string{"magic": srv2.URL}}
	if _, err := c.SiteStores(context.Background(), "magic"); err == nil {
		t.Error("a bad body decoded")
	}
}

func TestSiteStoreClientIsSafeForConcurrentUse(t *testing.T) {
	site := newFakeStores().sites["magic"]
	site.Game = ""
	body, _ := json.Marshal(site)
	var status atomic.Int32
	status.Store(http.StatusOK)
	srv, _ := storesServer(t, &status, string(body))
	c := &SiteStoreClient{Origins: map[string]string{"magic": srv.URL, "pokemon": srv.URL}, TTL: time.Nanosecond}
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			game := []string{"magic", "pokemon"}[i%2]
			if _, err := c.SiteStores(context.Background(), game); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

func TestNewSiteStoreClientUsesTheUpstreamOrigins(t *testing.T) {
	c := NewSiteStoreClient(&config.Config{Games: map[string]config.Game{
		"magic":   {Upstream: "https://www.mtgban.com/", Secret: "s"},
		"pokemon": {Upstream: "http://localhost:8081/some/path?x=1", Secret: "s"},
	}}, nil)
	want := map[string]string{"magic": "https://www.mtgban.com", "pokemon": "http://localhost:8081"}
	if !reflect.DeepEqual(c.Origins, want) || c.TTL != 5*time.Minute || c.HTTP == nil || c.HTTP.Timeout != 5*time.Second {
		t.Errorf("client %+v", c)
	}
}

func TestSiteStoreClientBacksOffAfterAFailure(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusBadGateway)
	srv, hits := storesServer(t, &status, "")
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	c := &SiteStoreClient{Origins: map[string]string{"magic": srv.URL}, Now: func() time.Time { return now }}
	ctx := context.Background()
	if _, err := c.SiteStores(ctx, "magic"); err == nil {
		t.Fatal("first failure succeeded")
	}
	now = now.Add(29 * time.Second)
	if _, err := c.SiteStores(ctx, "magic"); err == nil || hits.Load() != 1 {
		t.Errorf("within the backoff: err %v, %d requests", err, hits.Load())
	}
	now = now.Add(2 * time.Second)
	_, _ = c.SiteStores(ctx, "magic")
	if hits.Load() != 2 {
		t.Errorf("after the backoff: %d requests", hits.Load())
	}
}

func TestSiteStoreClientFetchesOnceForABurst(t *testing.T) {
	body, _ := json.Marshal(newFakeStores().sites["magic"])
	release := make(chan struct{})
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		<-release
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	c := &SiteStoreClient{Origins: map[string]string{"magic": srv.URL}}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.SiteStores(context.Background(), "magic"); err != nil {
				t.Error(err)
			}
		}()
	}
	for hits.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if hits.Load() != 1 {
		t.Errorf("%d fetches for one burst", hits.Load())
	}
}

func TestSiteStoreClientAlertsOnceWhenStaleForAnHour(t *testing.T) {
	body, _ := json.Marshal(newFakeStores().sites["magic"])
	var status atomic.Int32
	status.Store(http.StatusOK)
	srv, _ := storesServer(t, &status, string(body))
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	var alerts []string
	c := &SiteStoreClient{Origins: map[string]string{"magic": srv.URL}, Now: func() time.Time { return now },
		Alert: func(msg string) { alerts = append(alerts, msg) }}
	ctx := context.Background()
	_, _ = c.SiteStores(ctx, "magic")
	status.Store(http.StatusInternalServerError)
	for range 3 {
		now = now.Add(10 * time.Minute)
		_, _ = c.SiteStores(ctx, "magic")
	}
	if len(alerts) != 0 {
		t.Errorf("alerted before an hour: %v", alerts)
	}
	for range 5 {
		now = now.Add(10 * time.Minute)
		if _, err := c.SiteStores(ctx, "magic"); err != nil {
			t.Fatal(err)
		}
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts %v, want one", alerts)
	}
	status.Store(http.StatusOK)
	now = now.Add(time.Minute)
	_, _ = c.SiteStores(ctx, "magic")
	status.Store(http.StatusInternalServerError)
	now = now.Add(2 * time.Hour)
	_, _ = c.SiteStores(ctx, "magic")
	if len(alerts) != 2 {
		t.Errorf("a new failure window did not alert again: %v", alerts)
	}
}

func TestSiteStoreClientRejectsBadLists(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusOK)
	for name, body := range map[string]string{
		"other game": `{"game":"pokemon","implied":[{"key":"tcgplayer","name":"TCGplayer","shorthands":["TCGLow"]}]}`,
		"empty":      `{"game":"magic","implied":[],"stores":[]}`,
	} {
		srv, _ := storesServer(t, &status, body)
		c := &SiteStoreClient{Origins: map[string]string{"magic": srv.URL}}
		if _, err := c.SiteStores(context.Background(), "magic"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	srv, _ := storesServer(t, &status, `{"game":"","implied":[],"stores":[{"key":"CardKingdom","name":"Card Kingdom","shorthands":["CK"]}]}`)
	c := &SiteStoreClient{Origins: map[string]string{"magic": srv.URL}}
	if site, err := c.SiteStores(context.Background(), "magic"); err != nil || site.Stores[0].Key != "cardkingdom" {
		t.Errorf("keys not lowercased: %+v %v", site, err)
	}
}
