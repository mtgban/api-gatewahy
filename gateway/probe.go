package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"
)

// dbProbeName keys the database's entry alongside the game names in failing.
const dbProbeName = "database"

// probePingTimeout bounds the database ping so a stalled DB cannot hold up
// the tick and the upstream alerts already collected with it.
const probePingTimeout = 5 * time.Second

// Prober checks that each game accepts the gateway's signature, and that the
// database answers, alerting Discord on either one's state change.
type Prober struct {
	games  map[string]Upstream
	email  string
	link   string
	ttl    time.Duration
	now    func() time.Time
	client *http.Client
	pingDB func(context.Context) error
	alert  func(string)

	mu      sync.Mutex
	failing map[string]bool
}

// NewProber builds a prober. now, pingDB and alert may be nil; ttl must be positive,
// which serve.go gets from gateway.New's already-validated SigTTL.
func NewProber(games map[string]Upstream, email, link string, ttl time.Duration, now func() time.Time, client *http.Client, pingDB func(context.Context) error, alert func(string)) *Prober {
	if now == nil {
		now = time.Now
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if pingDB == nil {
		pingDB = func(context.Context) error { return nil }
	}
	if alert == nil {
		alert = func(string) {}
	}
	return &Prober{games: games, email: email, link: link, ttl: ttl, now: now, client: client, pingDB: pingDB, alert: alert, failing: map[string]bool{}}
}

// Check probes every game once and returns the failures by name.
func (p *Prober) Check(ctx context.Context) map[string]error {
	out := map[string]error{}
	for name, up := range p.games {
		out[name] = p.probe(ctx, name, up)
	}
	return out
}

// probeVersions are the API versions every game serves, each probed on its
// stores list.
var probeVersions = []string{"v1", "v2"}

// storesPath is the public path of a game's stores list in a version.
func storesPath(version, game string) string {
	return "/" + version + "/" + game + "/stores.json"
}

// probeBodyLimit bounds how much of a stores list the probe reads; a real
// one is a few KB.
const probeBodyLimit = 1 << 20

// probeRoute is the canonical stores-list route every game serves in a
// version, the same one live traffic uses, so its BackendPath can't drift
// from the probe's.
func probeRoute(version, game string) (Route, error) {
	return ParseRoute(storesPath(version, game))
}

// probe checks every version of a game, and fails naming the first that does.
func (p *Prober) probe(ctx context.Context, game string, up Upstream) error {
	for _, version := range probeVersions {
		err := p.probeVersion(ctx, version, game, up)
		if err != nil {
			return fmt.Errorf("%s: %w", version, err)
		}
	}
	return nil
}

func (p *Prober) probeVersion(ctx context.Context, version, game string, up Upstream) error {
	route, err := probeRoute(version, game)
	if err != nil {
		return err
	}
	sig := mintSig(up, p.link, p.email, "BASE_ACCESS", []string{"retail"}, p.now().Add(p.ttl))
	target := *up.URL
	target.Path = route.BackendPath()
	target.RawQuery = url.Values{"sig": {sig}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		// The upstream URL carries the signed query, so report the error class only.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		var oe *net.OpError
		if errors.As(err, &oe) && oe.Err != nil {
			err = oe.Err
		}
		return fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, probeBodyLimit))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	if !isStoresList(version, body) {
		return fmt.Errorf("unexpected body %q", body[:min(len(body), 64)])
	}
	return nil
}

// isStoresList reports whether body is a version's stores list: v1 sends an
// array, v2 an object holding both a sellers and a vendors array.
func isStoresList(version string, body []byte) bool {
	if version == "v1" {
		var list []json.RawMessage
		err := json.Unmarshal(body, &list)
		return err == nil && list != nil
	}
	var stores struct {
		Sellers []json.RawMessage `json:"sellers"`
		Vendors []json.RawMessage `json:"vendors"`
	}
	err := json.Unmarshal(body, &stores)
	return err == nil && stores.Sellers != nil && stores.Vendors != nil
}

// tick runs one check and alerts on each game, or the database, whose state changed.
func (p *Prober) tick(ctx context.Context) {
	results := p.Check(ctx)
	dctx, cancel := context.WithTimeout(ctx, probePingTimeout)
	results[dbProbeName] = p.pingDB(dctx)
	cancel()
	names := make([]string, 0, len(results))
	for name := range results {
		names = append(names, name)
	}
	sort.Strings(names)

	var messages []string
	p.mu.Lock()
	for _, name := range names {
		err := results[name]
		was := p.failing[name]
		now := err != nil
		p.failing[name] = now
		switch {
		case now && !was:
			messages = append(messages, fmt.Sprintf("api-gatewahy: probe of %s FAILING: %v", name, err))
		case !now && was:
			messages = append(messages, fmt.Sprintf("api-gatewahy: probe of %s recovered", name))
		}
	}
	p.mu.Unlock()

	for _, msg := range messages {
		p.alert(msg)
	}
}

// Run ticks every interval until ctx ends. The first tick is immediate.
func (p *Prober) Run(ctx context.Context, every time.Duration) {
	p.tick(ctx)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.tick(ctx)
		}
	}
}
