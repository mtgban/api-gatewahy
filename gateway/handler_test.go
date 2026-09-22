package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/mtgban-website/apisig"
)

type recordingMeter struct {
	mu   sync.Mutex
	rows []apiaccess.Usage
}

func (m *recordingMeter) Record(u apiaccess.Usage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows = append(m.rows, u)
}

// fakeBackend verifies the gateway signature like a game host would and
// echoes what it saw, so tests can assert the rewrite.
func fakeBackend(t *testing.T, secret string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/mtgban/retail/boom.json" {
			w.WriteHeader(500)
			return
		}
		if r.URL.Path == "/api/mtgban/retail/slow.json" {
			time.Sleep(500 * time.Millisecond)
		}
		if r.URL.Path == "/api/mtgban/retail/ratelimited.json" {
			w.WriteHeader(429)
			return
		}
		if r.URL.Path == "/api/mtgban/retail/redirect.json" {
			w.Header().Set("Location", "/api/mtgban/retail.json")
			w.WriteHeader(302)
			return
		}
		if r.URL.Path == "/api/mtgban/retail/notmodified.json" {
			w.WriteHeader(304)
			return
		}
		if r.URL.Path == "/api/mtgban/retail/errortext.json" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"meta": {"date": "2026-09-22"}, "retail": {"error": "invalid card", "regular": 1.5}}`))
			return
		}
		v, err := apisig.Decode(r.URL.Query().Get("sig"))
		if err != nil {
			_, _ = w.Write([]byte(`{"error": "invalid signature"}`))
			return
		}
		if err := apisig.Verify([]byte(secret), "GET", apisig.DefaultLink, v, []string{"APImode", "UserEmail"}, time.Now()); err != nil {
			_, _ = w.Write([]byte(`{"error": "invalid or expired signature"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"path": r.URL.Path, "api": v.Get("API"), "mode": v.Get("APImode"), "email": v.Get("UserEmail"),
			"xff": r.Header.Get("X-Forwarded-For"), "cookie": r.Header.Get("Cookie"), "auth": r.Header.Get("Authorization"),
			"clientheader": r.Header.Get(testClientIPHeader),
			"query":        r.URL.Query().Get("id"), "key": r.URL.Query().Get("key"), "host": r.Host,
		})
	}))
}

// testClientIPHeader is the header the test handler is configured to trust.
const testClientIPHeader = "DO-Connecting-IP"

const (
	goodKey    = "mtgban_live_abcdefghijklmnopqrstuvwxyz012345"
	revokedKey = "mtgban_live_revokedrevokedrevokedrevoked1234"
	suspKey    = "mtgban_live_suspendedsuspendedsuspended12345"
	unknownKey = "mtgban_live_00000000000000000000000000000000"
	devKey     = "mtgban_live_devdevdevdevdevdevdevdevdevdev12"
)

func testHandler(t *testing.T, backend *httptest.Server, secret string) (*Handler, *recordingMeter) {
	t.Helper()
	u, _ := url.Parse(backend.URL)
	revokedAt := now.Add(-time.Hour)
	src := &fakeSource{res: map[string]apiaccess.Lookup{
		apiaccess.HashKey(goodKey): {
			Key:     apiaccess.Key{ID: 7},
			Account: apiaccess.Account{ID: 3, Status: "active"},
			Entitlements: []apiaccess.Entitlement{
				ent([]string{"magic"}, "BASE_ACCESS", "retail", "buylist"),
				ent([]string{"pokemon"}, "CK,TCG", "retail"),
			},
		},
		apiaccess.HashKey(revokedKey): {
			Key:     apiaccess.Key{ID: 8, RevokedAt: &revokedAt},
			Account: apiaccess.Account{ID: 3, Status: "active"},
		},
		apiaccess.HashKey(suspKey): {
			Key:     apiaccess.Key{ID: 9},
			Account: apiaccess.Account{ID: 4, Status: "suspended"},
		},
		apiaccess.HashKey(devKey): {
			Key:          apiaccess.Key{ID: 10},
			Account:      apiaccess.Account{ID: 5, Status: "active"},
			Entitlements: []apiaccess.Entitlement{ent([]string{"magic"}, "DEV_ACCESS", "retail")},
		},
	}}
	meter := &recordingMeter{}
	h := New(Options{
		Games:           map[string]Upstream{"magic": {URL: u, Secret: []byte(secret)}, "pokemon": {URL: u, Secret: []byte(secret)}},
		GatewayEmail:    "gateway@mtgban.com",
		Link:            apisig.DefaultLink,
		ClientIPHeader:  testClientIPHeader,
		PerKeyRate:      1000,
		PerKeyBurst:     1000,
		PerIPRate:       100000,
		PerIPBurst:      100000,
		UpstreamTimeout: 100 * time.Millisecond,
		SigTTL:          5 * time.Minute,
		Now:             time.Now,
	}, NewResolver(src, time.Minute, nil), meter)
	return h, meter
}

func do(h http.Handler, method, target, key string) (*httptest.ResponseRecorder, map[string]any) {
	req := httptest.NewRequest(method, target, nil)
	req.RemoteAddr = "198.51.100.9:1234"
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Cookie", "MTGBAN=secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec, body
}

func TestHandlerHappyPath(t *testing.T) {
	be := fakeBackend(t, "s3cret")
	defer be.Close()
	h, meter := testHandler(t, be, "s3cret")

	rec, body := do(h, "GET", "/v1/magic/mtgban/retail/NEO.json?id=tcg&sig=stale&key=whatever", goodKey)
	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if body["path"] != "/api/mtgban/retail/NEO.json" || body["api"] != "BASE_ACCESS" || body["mode"] != "retail,buylist" ||
		body["email"] != "gateway@mtgban.com" || body["xff"] != "198.51.100.9" || body["cookie"] != "" || body["auth"] != "" ||
		body["query"] != "tcg" || body["key"] != "" {
		t.Errorf("backend saw %v", body)
	}
	if rec.Header().Get("X-MTGBAN-Game") != "magic" || rec.Header().Get("X-MTGBAN-Account") != "3" {
		t.Errorf("headers %v", rec.Header())
	}
	if len(meter.rows) != 1 || meter.rows[0].Status != 200 || meter.rows[0].KeyID != 7 || meter.rows[0].Game != "magic" ||
		meter.rows[0].Path != "/api/mtgban/retail/NEO.json" || meter.rows[0].Bytes == 0 || meter.rows[0].ClientIP != "198.51.100.9" {
		t.Errorf("meter %+v", meter.rows)
	}
}

func TestHandlerKeyInQuery(t *testing.T) {
	be := fakeBackend(t, "s3cret")
	defer be.Close()
	h, _ := testHandler(t, be, "s3cret")
	rec, _ := do(h, "GET", "/v1/pokemon/mtgban/retail.json?key="+goodKey, "")
	if rec.Code != 200 {
		t.Errorf("status %d body %s", rec.Code, rec.Body.String())
	}
}

func TestHandlerIgnoresInboundForwardedFor(t *testing.T) {
	be := fakeBackend(t, "s3cret")
	defer be.Close()
	h, meter := testHandler(t, be, "s3cret")
	req := httptest.NewRequest("GET", "/v1/magic/mtgban/retail.json", nil)
	req.RemoteAddr = "198.51.100.9:1234"
	req.Header.Set("Authorization", "Bearer "+goodKey)
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["xff"] != "198.51.100.9" {
		t.Errorf("backend saw xff %v, want the peer address", body["xff"])
	}
	if len(meter.rows) != 1 || meter.rows[0].ClientIP != "198.51.100.9" {
		t.Errorf("meter %+v", meter.rows)
	}
}

func TestHandlerHonorsClientIPHeader(t *testing.T) {
	be := fakeBackend(t, "s3cret")
	defer be.Close()
	h, meter := testHandler(t, be, "s3cret")
	req := httptest.NewRequest("GET", "/v1/magic/mtgban/retail.json", nil)
	req.RemoteAddr = "198.51.100.9:1234"
	req.Header.Set("Authorization", "Bearer "+goodKey)
	req.Header.Set(testClientIPHeader, "203.0.113.7")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["xff"] != "203.0.113.7" {
		t.Errorf("backend saw xff %v, want the trusted header value", body["xff"])
	}
	if body["clientheader"] != "" {
		t.Errorf("client header forwarded as %v", body["clientheader"])
	}
	if len(meter.rows) != 1 || meter.rows[0].ClientIP != "203.0.113.7" {
		t.Errorf("meter %+v", meter.rows)
	}
}

func TestHandlerRejectsUnparseableClientIP(t *testing.T) {
	be := fakeBackend(t, "s3cret")
	defer be.Close()
	h, meter := testHandler(t, be, "s3cret")
	req := httptest.NewRequest("GET", "/v1/magic/mtgban/retail.json", nil)
	req.RemoteAddr = "198.51.100.9:1234"
	req.Header.Set("Authorization", "Bearer "+goodKey)
	req.Header.Set(testClientIPHeader, "potato")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["xff"] != "198.51.100.9" {
		t.Errorf("backend saw xff %v, want the peer address", body["xff"])
	}
	if len(meter.rows) != 1 || meter.rows[0].ClientIP != "198.51.100.9" {
		t.Errorf("meter %+v", meter.rows)
	}
}

func TestHandlerRejectsZonedClientIP(t *testing.T) {
	be := fakeBackend(t, "s3cret")
	defer be.Close()
	h, meter := testHandler(t, be, "s3cret")
	req := httptest.NewRequest("GET", "/v1/magic/mtgban/retail.json", nil)
	req.RemoteAddr = "198.51.100.9:1234"
	req.Header.Set("Authorization", "Bearer "+goodKey)
	req.Header.Set(testClientIPHeader, "fe80::1%eth0")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["xff"] != "198.51.100.9" {
		t.Errorf("backend saw xff %v, want the peer address", body["xff"])
	}
	if len(meter.rows) != 1 || meter.rows[0].ClientIP != "198.51.100.9" {
		t.Errorf("meter %+v", meter.rows)
	}
}

func TestHandlerDevAccessScope(t *testing.T) {
	be := fakeBackend(t, "s3cret")
	defer be.Close()
	h, meter := testHandler(t, be, "s3cret")
	rec, body := do(h, "GET", "/v1/magic/mtgban/retail.json", devKey)
	if rec.Code != 403 || body["error"] != "plan has no store scope" || body["game"] != "magic" {
		t.Errorf("status %d body %v", rec.Code, body)
	}
	if len(meter.rows) != 0 {
		t.Errorf("dev access was metered: %+v", meter.rows)
	}
}

func TestHandlerRejections(t *testing.T) {
	be := fakeBackend(t, "s3cret")
	defer be.Close()
	h, meter := testHandler(t, be, "s3cret")

	cases := []struct {
		name, method, target, key string
		status                    int
		errContains, game         string
	}{
		{"post", "POST", "/v1/magic/mtgban/retail.json", goodKey, 405, "method", ""},
		{"bad path", "GET", "/v1/magic/nope.json", goodKey, 404, "not found", ""},
		{"unknown game", "GET", "/v1/lorcana/mtgban/retail.json", goodKey, 404, "unknown game", "lorcana"},
		{"no key", "GET", "/v1/magic/mtgban/retail.json", "", 401, "missing API key", ""},
		{"malformed key", "GET", "/v1/magic/mtgban/retail.json", "abc", 401, "malformed API key", ""},
		{"unknown key", "GET", "/v1/magic/mtgban/retail.json", unknownKey, 401, "unknown API key", ""},
		{"revoked", "GET", "/v1/magic/mtgban/retail.json", revokedKey, 401, "revoked", ""},
		{"suspended", "GET", "/v1/magic/mtgban/retail.json", suspKey, 401, "suspended", ""},
		{"pokemon lacks sealed", "GET", "/v1/pokemon/mtgban/sealed.json", goodKey, 403, "plan does not include mode sealed", "pokemon"},
		{"mode not in plan", "GET", "/v1/magic/mtgban/sealed.json", goodKey, 403, "plan does not include mode sealed", "magic"},
		{"all needs both", "GET", "/v1/pokemon/mtgban/all.json", goodKey, 403, "buylist", "pokemon"},
	}
	for _, c := range cases {
		rec, body := do(h, c.method, c.target, c.key)
		if rec.Code != c.status {
			t.Errorf("%s: status %d want %d (%s)", c.name, rec.Code, c.status, rec.Body.String())
			continue
		}
		if msg, _ := body["error"].(string); !strings.Contains(msg, c.errContains) {
			t.Errorf("%s: error %q want containing %q", c.name, msg, c.errContains)
		}
		if g, _ := body["game"].(string); g != c.game {
			t.Errorf("%s: game %q want %q", c.name, g, c.game)
		}
		if rec.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s: content type %q", c.name, rec.Header().Get("Content-Type"))
		}
	}
	if len(meter.rows) != 0 {
		t.Errorf("rejections were metered: %+v", meter.rows)
	}
}

func TestHandlerNoEntitlementForGame(t *testing.T) {
	be := fakeBackend(t, "s3cret")
	defer be.Close()
	h, _ := testHandler(t, be, "s3cret")
	// The account has magic and pokemon; add a third configured game with no entitlement.
	u, _ := url.Parse(be.URL)
	h.games["lorcana"] = Upstream{URL: u, Secret: []byte("s3cret")}
	h.proxies["lorcana"] = h.newProxy("lorcana", h.games["lorcana"])
	rec, body := do(h, "GET", "/v1/lorcana/mtgban/sets.json", goodKey)
	if rec.Code != 403 || body["error"] != "plan does not include game" || body["game"] != "lorcana" {
		t.Errorf("status %d body %v", rec.Code, body)
	}
}

func TestHandlerUnavailable(t *testing.T) {
	be := fakeBackend(t, "s3cret")
	defer be.Close()
	u, _ := url.Parse(be.URL)
	src := &fakeSource{err: context.DeadlineExceeded}
	h := New(Options{Games: map[string]Upstream{"magic": {URL: u, Secret: []byte("s")}}, GatewayEmail: "g@x", Link: apisig.DefaultLink,
		PerKeyRate: 10, PerKeyBurst: 5, UpstreamTimeout: time.Second, SigTTL: time.Minute}, NewResolver(src, time.Minute, nil), &recordingMeter{})
	rec, body := do(h, "GET", "/v1/magic/mtgban/retail.json", goodKey)
	if rec.Code != 503 || rec.Header().Get("Retry-After") != "30" || body["error"] == nil {
		t.Errorf("status %d headers %v body %v", rec.Code, rec.Header(), body)
	}
}

func TestHandlerUpstreamFailures(t *testing.T) {
	be := fakeBackend(t, "s3cret")
	defer be.Close()

	t.Run("wrong secret becomes 502", func(t *testing.T) {
		h, meter := testHandler(t, be, "wrong-secret")
		rec, body := do(h, "GET", "/v1/magic/mtgban/retail.json", goodKey)
		if rec.Code != 502 || !strings.Contains(body["error"].(string), "rejected gateway signature") {
			t.Errorf("status %d body %v", rec.Code, body)
		}
		if len(meter.rows) != 1 || meter.rows[0].Status != 502 {
			t.Errorf("meter %+v", meter.rows)
		}
	})
	t.Run("upstream 500 becomes 502", func(t *testing.T) {
		h, _ := testHandler(t, be, "s3cret")
		rec, body := do(h, "GET", "/v1/magic/mtgban/retail/boom.json", goodKey)
		if rec.Code != 502 || !strings.Contains(body["error"].(string), "500") {
			t.Errorf("status %d body %v", rec.Code, body)
		}
	})
	t.Run("upstream 302 becomes 502", func(t *testing.T) {
		h, _ := testHandler(t, be, "s3cret")
		rec, body := do(h, "GET", "/v1/magic/mtgban/retail/redirect.json", goodKey)
		if rec.Code != 502 || !strings.Contains(body["error"].(string), "302") {
			t.Errorf("status %d body %v", rec.Code, body)
		}
	})
	t.Run("upstream 429 passes through", func(t *testing.T) {
		h, _ := testHandler(t, be, "s3cret")
		rec, _ := do(h, "GET", "/v1/magic/mtgban/retail/ratelimited.json", goodKey)
		if rec.Code != 429 {
			t.Errorf("status %d", rec.Code)
		}
	})
	t.Run("upstream 304 passes through", func(t *testing.T) {
		h, _ := testHandler(t, be, "s3cret")
		rec, _ := do(h, "GET", "/v1/magic/mtgban/retail/notmodified.json", goodKey)
		if rec.Code != 304 {
			t.Errorf("status %d", rec.Code)
		}
	})
	t.Run("timeout becomes 504", func(t *testing.T) {
		h, _ := testHandler(t, be, "s3cret")
		rec, body := do(h, "GET", "/v1/magic/mtgban/retail/slow.json", goodKey)
		if rec.Code != 504 || !strings.Contains(body["error"].(string), "timed out") {
			t.Errorf("status %d body %v", rec.Code, body)
		}
	})
	t.Run("connection refused becomes 502", func(t *testing.T) {
		dead := httptest.NewServer(http.NotFoundHandler())
		dead.Close()
		h, _ := testHandler(t, dead, "s3cret")
		rec, body := do(h, "GET", "/v1/magic/mtgban/retail.json", goodKey)
		if rec.Code != 502 || !strings.Contains(body["error"].(string), "unavailable") {
			t.Errorf("status %d body %v", rec.Code, body)
		}
	})
}

func TestHandlerRateLimit(t *testing.T) {
	be := fakeBackend(t, "s3cret")
	defer be.Close()
	h, _ := testHandler(t, be, "s3cret")
	h.limiter = newLimiter(1, 1)
	do(h, "GET", "/v1/magic/mtgban/retail.json", goodKey)
	rec, _ := do(h, "GET", "/v1/magic/mtgban/retail.json", goodKey)
	if rec.Code != 429 || rec.Header().Get("RateLimit-Limit") != "1" {
		t.Errorf("status %d headers %v", rec.Code, rec.Header())
	}
}

// abortTransport answers 200 with a body that dies after ten bytes, which is
// what makes ReverseProxy abort the copy.
type abortTransport struct{}

func (abortTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body := io.MultiReader(strings.NewReader("0123456789"), iotest.ErrReader(errors.New("stream broke")))
	return &http.Response{
		Status:        "200 OK",
		StatusCode:    http.StatusOK,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": {"application/json"}},
		Body:          io.NopCloser(body),
		ContentLength: -1,
		Request:       r,
	}, nil
}

func TestHandlerMetersAbortedResponse(t *testing.T) {
	be := fakeBackend(t, "s3cret")
	defer be.Close()
	h, meter := testHandler(t, be, "s3cret")
	h.proxies["magic"].Transport = abortTransport{}

	req := httptest.NewRequest("GET", "/v1/magic/mtgban/retail.json", nil)
	req.RemoteAddr = "198.51.100.9:1234"
	req.Header.Set("Authorization", "Bearer "+goodKey)
	// ReverseProxy only panics when it believes an http.Server will recover.
	req = req.WithContext(context.WithValue(req.Context(), http.ServerContextKey, &http.Server{}))
	rec := httptest.NewRecorder()

	func() {
		defer func() {
			if p := recover(); p != http.ErrAbortHandler {
				t.Errorf("recovered %v, want http.ErrAbortHandler", p)
			}
		}()
		h.ServeHTTP(rec, req)
	}()

	if len(meter.rows) != 1 || meter.rows[0].Status != 200 || meter.rows[0].Bytes != 10 ||
		meter.rows[0].KeyID != 7 || meter.rows[0].Path != "/api/mtgban/retail.json" {
		t.Errorf("meter %+v", meter.rows)
	}
}

func TestHandlerMissingProxyIsNotMetered(t *testing.T) {
	be := fakeBackend(t, "s3cret")
	defer be.Close()
	h, meter := testHandler(t, be, "s3cret")
	// A game configured without a proxy must not nil-deref.
	u, _ := url.Parse(be.URL)
	h.games["lorcana"] = Upstream{URL: u, Secret: []byte("s3cret")}
	rec, body := do(h, "GET", "/v1/lorcana/mtgban/retail.json", goodKey)
	if rec.Code != 404 || body["error"] != "unknown game" || body["game"] != "lorcana" {
		t.Errorf("status %d body %v", rec.Code, body)
	}
	if len(meter.rows) != 0 {
		t.Errorf("meter %+v", meter.rows)
	}
}

func TestBoomPathIsNotAccidentallyCached(t *testing.T) {
	// Sanity check for the test backend itself.
	be := fakeBackend(t, "s3cret")
	defer be.Close()
	resp, err := http.Get(be.URL + "/api/mtgban/retail/boom.json")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Errorf("fake backend boom returned %d", resp.StatusCode)
	}
}

func TestSigRejectedIsStructural(t *testing.T) {
	for body, want := range map[string]bool{
		`{"error": "invalid signature"}`:                    true,
		`{"error":"invalid or expired signature"}`:          true,
		"  " + `{"error": "invalid signature"}`:             true,
		`{"meta": {}, "retail": {"error": "invalid card"}}`: false,
		`{"error": "", "meta": {}}`:                         false,
		"":                                                  false,
	} {
		if got := sigRejected([]byte(body)); got != want {
			t.Errorf("%q: %v", body, got)
		}
	}
}

func TestErrorTextInsideDataPassesThrough(t *testing.T) {
	backend := fakeBackend(t, "secret")
	defer backend.Close()
	h, _ := testHandler(t, backend, "secret")
	rec, body := do(h, "GET", "/v1/magic/mtgban/retail/errortext.json", goodKey)
	if rec.Code != 200 || body["retail"] == nil {
		t.Fatalf("legitimate body treated as a signature rejection: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPerIPLimitRunsBeforeAnyLookup(t *testing.T) {
	backend := fakeBackend(t, "secret")
	defer backend.Close()
	u, _ := url.Parse(backend.URL)
	src := &fakeSource{res: map[string]apiaccess.Lookup{}}
	h := New(Options{
		Games:          map[string]Upstream{"magic": {URL: u, Secret: []byte("secret")}},
		GatewayEmail:   "gateway@mtgban.com",
		Link:           apisig.DefaultLink,
		ClientIPHeader: testClientIPHeader,
		PerKeyRate:     1000,
		PerKeyBurst:    1000,
		PerIPRate:      1,
		PerIPBurst:     3,
	}, NewResolver(src, time.Minute, nil), &recordingMeter{})
	for i := 0; i < 3; i++ {
		fake := "mtgban_live_" + strings.Repeat(string(rune('a'+i)), 32)
		if rec, _ := do(h, "GET", "/v1/magic/mtgban/retail.json", fake); rec.Code != 401 {
			t.Fatalf("forged key %d: %d", i, rec.Code)
		}
	}
	rec, _ := do(h, "GET", "/v1/magic/mtgban/retail.json", "mtgban_live_"+strings.Repeat("z", 32))
	if rec.Code != 429 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("fourth forged key from one address: %d", rec.Code)
	}
	if src.calls != 3 {
		t.Errorf("lookups %d, want 3: the limiter must run before the database", src.calls)
	}
	// Another address is unaffected.
	req := httptest.NewRequest("GET", "/v1/magic/mtgban/retail.json", nil)
	req.RemoteAddr = "203.0.113.7:1234"
	req.Header.Set("Authorization", "Bearer mtgban_live_"+strings.Repeat("q", 32))
	other := httptest.NewRecorder()
	h.ServeHTTP(other, req)
	if other.Code != 401 {
		t.Errorf("other address: %d", other.Code)
	}
}

func TestClientIPTakesTheLastHeaderValue(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "198.51.100.9:1234"
	req.Header.Set("X-Forwarded-For", "10.0.0.1, 203.0.113.5")
	if got := ClientIP(req, "X-Forwarded-For"); got != "203.0.113.5" {
		t.Errorf("last hop: %q", got)
	}
	req.Header.Set("X-Forwarded-For", "not-an-ip")
	if got := ClientIP(req, "X-Forwarded-For"); got != "198.51.100.9" {
		t.Errorf("fallback: %q", got)
	}
}
