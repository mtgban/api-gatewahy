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
		_, _ = w.Write([]byte(storesBody(r.URL.Path)))
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

// storesBody is a backend's stores list, in the shape of the version path
// asks for.
func storesBody(path string) string {
	if strings.HasPrefix(path, "/api/v2/") {
		return `{"sellers":[{"shorthand":"CK","name":"Card Kingdom"}],"vendors":[]}`
	}
	return `["TCG","CK"]`
}

// TestProberChecksV2 fails a game whose host answers v1 but not with a v2
// stores list, as one that predates the v2 API would, and says which.
func TestProberChecksV2(t *testing.T) {
	cases := []struct {
		name, v2 string
		ok       bool
	}{
		{"stores list", storesBody("/api/v2/stores.json"), true},
		{"no stores", `{"sellers":[],"vendors":[]}`, true},
		{"not the API", `<html>not the API</html>`, false},
		{"signature error", `{"error": "invalid or expired signature"}`, false},
		{"v1 shape", `["CK"]`, false},
		{"no vendors", `{"sellers":[]}`, false},
		{"null", `null`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/api/v2/") {
					_, _ = w.Write([]byte(c.v2))
					return
				}
				_, _ = w.Write([]byte(storesBody(r.URL.Path)))
			}))
			defer srv.Close()
			u, _ := url.Parse(srv.URL)
			p := NewProber(map[string]Upstream{"magic": {URL: u, Secret: []byte("s")}}, "g@x", apisig.DefaultLink, 5*time.Minute, nil, srv.Client(), nil, nil)
			err := p.Check(context.Background())["magic"]
			if c.ok && err != nil {
				t.Errorf("probe failed: %v", err)
			}
			if !c.ok && (err == nil || !strings.HasPrefix(err.Error(), "v2: ")) {
				t.Errorf("probe of %s: %v", c.v2, err)
			}
		})
	}
}

func TestProberAlertsOnTransition(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if healthy.Load() {
			_, _ = w.Write([]byte(storesBody(r.URL.Path)))
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
