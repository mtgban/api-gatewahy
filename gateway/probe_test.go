package gateway

import (
	"context"
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
	}, "gateway@mtgban.com", apisig.DefaultLink, good.Client(), nil)
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
	p := NewProber(map[string]Upstream{"magic": {URL: u, Secret: []byte("s")}}, "g@x", apisig.DefaultLink, srv.Client(),
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

func TestProberErrorHidesSignature(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	u, _ := url.Parse(srv.URL)
	srv.Close()

	p := NewProber(map[string]Upstream{"magic": {URL: u, Secret: []byte("s")}}, "g@x", apisig.DefaultLink, srv.Client(), nil)
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
