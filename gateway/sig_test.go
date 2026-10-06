package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/mtgban-website/apisig"
)

// recordedRequest is the path and sig one request carried.
type recordedRequest struct {
	path string
	sig  string
}

// sigRecorder records each request's path and sig and answers with a
// minimal JSON array so both the handler's proxy and the prober accept it.
type sigRecorder struct {
	mu   sync.Mutex
	reqs []recordedRequest
}

func (s *sigRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.reqs = append(s.reqs, recordedRequest{path: r.URL.Path, sig: r.URL.Query().Get("sig")})
	s.mu.Unlock()
	_, _ = w.Write([]byte("[]"))
}

// TestMintSigMatchesHandlerAndProber fails if the handler and the prober
// ever again send a different path or a differently-built signature.
func TestMintSigMatchesHandlerAndProber(t *testing.T) {
	rec := &sigRecorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	fixedNow := func() time.Time { return now }
	const ttl = 5 * time.Minute
	up := Upstream{URL: u, Secret: []byte("shared-secret")}

	src := &fakeSource{res: map[string]apiaccess.Lookup{
		apiaccess.HashKey(goodKey): {
			Key:          apiaccess.Key{ID: 1},
			Account:      apiaccess.Account{ID: 1, Status: "active"},
			Entitlements: []apiaccess.Entitlement{ent([]string{"magic"}, "BASE_ACCESS", "retail")},
		},
	}}
	h := mustNew(t, Options{
		Games:           map[string]Upstream{"magic": up},
		GatewayEmail:    "gateway@mtgban.com",
		Link:            apisig.DefaultLink,
		PerKeyRate:      1000,
		PerKeyBurst:     1000,
		PerIPRate:       1000,
		PerIPBurst:      1000,
		UpstreamTimeout: time.Second,
		SigTTL:          ttl,
		Now:             fixedNow,
	}, NewResolver(src, time.Minute, nil), &recordingMeter{})

	hRec, _ := do(h, "GET", "/v1/magic/stores.json", goodKey)
	if hRec.Code != 200 {
		t.Fatalf("handler request status %d", hRec.Code)
	}

	p := NewProber(map[string]Upstream{"magic": up}, "gateway@mtgban.com", apisig.DefaultLink, ttl, fixedNow, srv.Client(), nil, nil)
	if errs := p.Check(context.Background()); errs["magic"] != nil {
		t.Fatalf("prober check: %v", errs["magic"])
	}

	rec.mu.Lock()
	reqs := append([]recordedRequest(nil), rec.reqs...)
	rec.mu.Unlock()
	if len(reqs) != 2 {
		t.Fatalf("recorded %d requests, want 2: %v", len(reqs), reqs)
	}
	handlerReq, proberReq := reqs[0], reqs[1]

	// Same secret, link, scope, modes, and clock: the HMAC is deterministic,
	// so an equal sig proves every claim matched; the path is checked apart.
	if handlerReq.path != proberReq.path {
		t.Errorf("path differs: handler %q prober %q", handlerReq.path, proberReq.path)
	}
	if handlerReq.sig != proberReq.sig {
		t.Errorf("sig differs: handler %q prober %q", handlerReq.sig, proberReq.sig)
	}
}
