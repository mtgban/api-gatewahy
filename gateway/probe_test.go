package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mtgban/mtgban-website/apisig"
)

func TestProberCheck(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, _ := apisig.Decode(r.URL.Query().Get("sig"))
		if apisig.Verify([]byte("ok"), "GET", apisig.DefaultLink, v, []string{"APImode", "UserEmail"}, time.Now()) != nil {
			_, _ = w.Write([]byte(`{"error": "invalid or expired signature"}`))
			return
		}
		_, _ = w.Write([]byte(`["TCG","CK"]`))
	}))
	defer good.Close()
	gu, _ := url.Parse(good.URL)

	p := NewProber(map[string]Upstream{
		"magic":   {URL: gu, Secret: []byte("ok")},
		"pokemon": {URL: gu, Secret: []byte("wrong")},
	}, "gateway@mtgban.com", apisig.DefaultLink, 5*time.Minute, nil, good.Client(), nil, nil)
	errs := p.Check(context.Background())
	if errs["magic"] != nil {
		t.Errorf("magic: %v", errs["magic"])
	}
	if errs["pokemon"] == nil {
		t.Error("pokemon with wrong secret passed")
	}
}

func TestProberAlertsOnTransition(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if healthy.Load() {
			_, _ = w.Write([]byte(`[]`))
		} else {
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	var alerts []string
	p := NewProber(map[string]Upstream{"magic": {URL: u, Secret: []byte("s")}}, "g@x", apisig.DefaultLink, 5*time.Minute, nil, srv.Client(), nil,
		func(msg string) { alerts = append(alerts, msg) })

	p.tick(context.Background())
	p.tick(context.Background())
	healthy.Store(false)
	p.tick(context.Background())
	p.tick(context.Background())
	healthy.Store(true)
	p.tick(context.Background())
	if len(alerts) != 2 {
		t.Fatalf("alerts %v", alerts)
	}
}

func TestProberAlertsOnDBTransition(t *testing.T) {
	var up atomic.Bool
	up.Store(true)
	pingDB := func(context.Context) error {
		if up.Load() {
			return nil
		}
		return errors.New("connection refused")
	}
	var alerts []string
	p := NewProber(nil, "g@x", apisig.DefaultLink, 5*time.Minute, nil, nil, pingDB, func(msg string) { alerts = append(alerts, msg) })

	p.tick(context.Background())
	p.tick(context.Background())
	up.Store(false)
	p.tick(context.Background())
	p.tick(context.Background())
	up.Store(true)
	p.tick(context.Background())
	if len(alerts) != 2 {
		t.Fatalf("alerts %v", alerts)
	}
	if !strings.Contains(alerts[0], "database") || !strings.Contains(alerts[0], "FAILING") {
		t.Errorf("first alert %q", alerts[0])
	}
	if !strings.Contains(alerts[1], "database") || !strings.Contains(alerts[1], "recovered") {
		t.Errorf("second alert %q", alerts[1])
	}
}

func TestProberAlertsOnceWhenDBStartsDown(t *testing.T) {
	pingDB := func(context.Context) error { return errors.New("connection refused") }
	var alerts []string
	p := NewProber(nil, "g@x", apisig.DefaultLink, 5*time.Minute, nil, nil, pingDB, func(msg string) { alerts = append(alerts, msg) })

	p.tick(context.Background())
	p.tick(context.Background())
	p.tick(context.Background())
	if len(alerts) != 1 {
		t.Fatalf("alerts %v", alerts)
	}
	if !strings.Contains(alerts[0], "database") || !strings.Contains(alerts[0], "FAILING") {
		t.Errorf("alert %q", alerts[0])
	}
}

func TestProberBoundsDBPing(t *testing.T) {
	var hadDeadline bool
	var deadline time.Time
	pingDB := func(ctx context.Context) error {
		deadline, hadDeadline = ctx.Deadline()
		return nil
	}
	p := NewProber(nil, "g@x", apisig.DefaultLink, 5*time.Minute, nil, nil, pingDB, nil)

	start := time.Now()
	p.tick(context.Background())
	if !hadDeadline {
		t.Fatal("pingDB ran with no deadline")
	}
	if d := deadline.Sub(start); d < probePingTimeout-time.Second || d > probePingTimeout+time.Second {
		t.Errorf("deadline %s from start, want close to %s", d, probePingTimeout)
	}
}

func TestProberErrorHidesSignature(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	u, _ := url.Parse(srv.URL)
	srv.Close()

	p := NewProber(map[string]Upstream{"magic": {URL: u, Secret: []byte("s")}}, "g@x", apisig.DefaultLink, 5*time.Minute, nil, srv.Client(), nil, nil)
	errs := p.Check(context.Background())
	err := errs["magic"]
	if err == nil {
		t.Fatal("expected error against a closed server")
	}
	msg := err.Error()
	if strings.Contains(msg, "sig=") {
		t.Errorf("error leaks the signed query: %s", msg)
	}
	if strings.Contains(msg, "stores.json") {
		t.Errorf("error leaks the upstream path: %s", msg)
	}
	if strings.Contains(msg, u.Host+"/api") {
		t.Errorf("error leaks the upstream host with the query: %s", msg)
	}
}
