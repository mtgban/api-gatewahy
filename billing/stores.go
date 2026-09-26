package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mtgban/api-gatewahy/config"
	"github.com/mtgban/mtgban-website/apiproductlist"
)

// StoresPath is where every game site lists its store families.
const StoresPath = "/api-plans/stores.json"

const (
	storesTimeout    = 5 * time.Second
	storesTTL        = 5 * time.Minute
	storesRetryAfter = 30 * time.Second
	storesStaleAlert = time.Hour
	maxStoresBody    = 1 << 20
)

// ErrStoresUnavailable means a game site's store list could not be loaded.
var ErrStoresUnavailable = errors.New("billing: store list unavailable")

// StoreFamily is one scraper config entry a site sells, with its backend shorthands.
type StoreFamily struct {
	Key        string   `json:"key"`
	Name       string   `json:"name"`
	Shorthands []string `json:"shorthands"`
}

// SiteStores is one game site's stores.json: implied families and selectable ones.
type SiteStores struct {
	Game    string        `json:"game"`
	Implied []StoreFamily `json:"implied"`
	Stores  []StoreFamily `json:"stores"`
}

// StoreLister returns a game site's store families; callers must not modify the result.
type StoreLister interface {
	SiteStores(ctx context.Context, game string) (SiteStores, error)
}

// SiteStoreClient fetches stores.json from each game's origin and caches it per game.
type SiteStoreClient struct {
	Origins map[string]string
	HTTP    *http.Client
	TTL     time.Duration
	Now     func() time.Time
	// Alert hears once per failure window when a site's list has been stale for an hour.
	Alert func(string)

	mu    sync.Mutex
	cache map[string]*siteEntry
}

// siteEntry is one game's cached list, last failure, and in-flight fetch.
type siteEntry struct {
	site     SiteStores
	at       time.Time
	has      bool
	failedAt time.Time
	err      error
	fetching chan struct{}
	alerted  bool
}

// NewSiteStoreClient reads each game's origin from its upstream URL.
func NewSiteStoreClient(cfg *config.Config, alert func(string)) *SiteStoreClient {
	origins := map[string]string{}
	for name, g := range cfg.Games {
		if u, err := url.Parse(g.Upstream); err == nil && u.Scheme != "" && u.Host != "" {
			origins[name] = u.Scheme + "://" + u.Host
		}
	}
	return &SiteStoreClient{Origins: origins, HTTP: &http.Client{Timeout: storesTimeout}, TTL: storesTTL, Alert: alert}
}

// SiteStores serves the cached list while fresh and the last good one when a refetch fails.
func (c *SiteStoreClient) SiteStores(ctx context.Context, game string) (SiteStores, error) {
	ttl := c.TTL
	if ttl <= 0 {
		ttl = storesTTL
	}
	c.mu.Lock()
	if c.cache == nil {
		c.cache = map[string]*siteEntry{}
	}
	e := c.cache[game]
	if e == nil {
		e = &siteEntry{}
		c.cache[game] = e
	}
	for {
		now := c.now()
		if e.has && now.Sub(e.at) < ttl {
			site := e.site
			c.mu.Unlock()
			return site, nil
		}
		if e.err != nil && now.Sub(e.failedAt) < storesRetryAfter {
			site, has, err := e.site, e.has, e.err
			c.mu.Unlock()
			if has {
				return site, nil
			}
			return SiteStores{}, err
		}
		if e.fetching == nil {
			break
		}
		// Another caller is fetching this game; wait for it and look again.
		done := e.fetching
		c.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return SiteStores{}, fmt.Errorf("%w: %w", ErrStoresUnavailable, ctx.Err())
		}
		c.mu.Lock()
	}
	done := make(chan struct{})
	e.fetching = done
	c.mu.Unlock()

	// The fetch outlives a caller that gives up, so one cancelled request does not count as a site failure.
	site, err := func() (SiteStores, error) {
		// Deferred so a panic in fetch still releases the waiters.
		defer func() {
			c.mu.Lock()
			e.fetching = nil
			close(done)
			c.mu.Unlock()
		}()
		return c.fetch(context.WithoutCancel(ctx), game)
	}()

	c.mu.Lock()
	now := c.now()
	if err == nil {
		e.site, e.at, e.has, e.err, e.alerted = site, now, true, nil, false
		c.mu.Unlock()
		return site, nil
	}
	e.err, e.failedAt = err, now
	stale, has, at := e.site, e.has, e.at
	alert := has && now.Sub(at) >= storesStaleAlert && !e.alerted
	if alert {
		e.alerted = true
	}
	c.mu.Unlock()
	if !has {
		return SiteStores{}, err
	}
	log.Printf("billing: stores for %s: %v; serving the list from %s", game, err, at.Format(time.RFC3339))
	if alert && c.Alert != nil {
		c.Alert(fmt.Sprintf("api-gatewahy: the %s store list has not refreshed since %s: %v", game, at.Format(time.RFC3339), err))
	}
	return stale, nil
}

func (c *SiteStoreClient) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *SiteStoreClient) fetch(ctx context.Context, game string) (SiteStores, error) {
	origin, ok := c.Origins[game]
	if !ok {
		return SiteStores{}, fmt.Errorf("billing: no site configured for game %q", game)
	}
	ctx, cancel := context.WithTimeout(ctx, storesTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(origin, "/")+StoresPath, nil)
	if err != nil {
		return SiteStores{}, err
	}
	req.Header.Set("Accept", "application/json")
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: storesTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return SiteStores{}, fmt.Errorf("billing: fetch %s stores: %w", game, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return SiteStores{}, fmt.Errorf("billing: fetch %s stores: status %d", game, resp.StatusCode)
	}
	var site SiteStores
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxStoresBody)).Decode(&site); err != nil {
		return SiteStores{}, fmt.Errorf("billing: decode %s stores: %w", game, err)
	}
	if site.Game != "" && site.Game != game {
		return SiteStores{}, fmt.Errorf("billing: %s stores: the site says it serves %q", game, site.Game)
	}
	if len(site.Implied) == 0 && len(site.Stores) == 0 {
		return SiteStores{}, fmt.Errorf("billing: %s stores: the site lists no stores", game)
	}
	for _, list := range [][]StoreFamily{site.Implied, site.Stores} {
		for i := range list {
			list[i].Key = strings.ToLower(list[i].Key)
		}
	}
	return site, nil
}

// ResolvedPlan is a plan with its entitlement store scope and store names.
type ResolvedPlan struct {
	Plan
	Scope string
	Modes []string
	// Implied are the implied family names; Names maps each picked key to its name.
	Implied []string
	Names   map[string]string
}

// StoreNames lists the implied families, then the picked ones in key order.
func (r ResolvedPlan) StoreNames() []string {
	out := slices.Clone(r.Implied)
	for _, k := range r.Stores {
		if n := r.Names[k]; n != "" {
			out = append(out, n)
		}
	}
	return out
}

// Resolve checks the picked keys against the plan's game sites and builds the entitlement scope.
func (p Plan) Resolve(ctx context.Context, cat *apiproductlist.ProductList, lister StoreLister) (ResolvedPlan, error) {
	pkg, ok := cat.Package(p.Package)
	if !ok {
		return ResolvedPlan{}, invalid("unknown package %q", p.Package)
	}
	r := ResolvedPlan{Plan: p, Scope: pkg.StoreScope, Modes: pkg.Modes}
	if pkg.StoreScope != apiproductlist.StoreScopeExplicit {
		return r, nil
	}
	if lister == nil {
		return ResolvedPlan{}, fmt.Errorf("%w: no store lister", ErrStoresUnavailable)
	}
	r.Names = map[string]string{}
	var shorthands []string
	for _, g := range p.Games {
		site, err := lister.SiteStores(ctx, g)
		if err != nil {
			return ResolvedPlan{}, fmt.Errorf("%w: %s: %w", ErrStoresUnavailable, g, err)
		}
		for _, f := range site.Implied {
			shorthands = append(shorthands, f.Shorthands...)
			if !slices.Contains(r.Implied, f.Name) {
				r.Implied = append(r.Implied, f.Name)
			}
		}
		for _, f := range site.Stores {
			if !slices.Contains(p.Stores, f.Key) {
				continue
			}
			shorthands = append(shorthands, f.Shorthands...)
			if _, seen := r.Names[f.Key]; !seen {
				r.Names[f.Key] = f.Name
			}
		}
	}
	for _, k := range p.Stores {
		if _, ok := r.Names[k]; !ok {
			return ResolvedPlan{}, &ValidationError{Msg: "Store " + k + " is not available for the games you picked."}
		}
	}
	shorthands = dedupe(shorthands, func(s string) string { return s })
	if len(shorthands) == 0 {
		return ResolvedPlan{}, fmt.Errorf("%w: the sites list no shorthands for %v", ErrStoresUnavailable, p.Stores)
	}
	slices.Sort(shorthands)
	r.Scope = strings.Join(shorthands, ",")
	return r, nil
}
