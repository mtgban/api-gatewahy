package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"

	"github.com/mtgban/mtgban-website/apisig"
)

// Prober checks that each game accepts the gateway's signature.
type Prober struct {
	games  map[string]Upstream
	email  string
	link   string
	client *http.Client
	alert  func(string)

	mu      sync.Mutex
	failing map[string]bool
}

// NewProber builds a prober. alert may be nil.
func NewProber(games map[string]Upstream, email, link string, client *http.Client, alert func(string)) *Prober {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if alert == nil {
		alert = func(string) {}
	}
	return &Prober{games: games, email: email, link: link, client: client, alert: alert, failing: map[string]bool{}}
}

// Check probes every game once and returns the failures by name.
func (p *Prober) Check(ctx context.Context) map[string]error {
	out := map[string]error{}
	for name, up := range p.games {
		out[name] = p.probe(ctx, up)
	}
	return out
}

func (p *Prober) probe(ctx context.Context, up Upstream) error {
	sig := apisig.Mint(up.Secret, p.link, apisig.Claims{
		API:     "BASE_ACCESS",
		Fields:  url.Values{"APImode": {"retail"}, "UserEmail": {p.email}},
		Expires: time.Now().Add(5 * time.Minute).Unix(),
	})
	target := *up.URL
	target.Path = "/api/mtgban/stores.json"
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
	head := make([]byte, 64)
	n, _ := io.ReadFull(resp.Body, head)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	if n == 0 || head[0] != '[' {
		return fmt.Errorf("unexpected body %q", string(head[:n]))
	}
	return nil
}

// tick runs one check and alerts on each game whose state changed.
func (p *Prober) tick(ctx context.Context) {
	results := p.Check(ctx)
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
