package portal

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/api-gatewahy/mailer"
	"github.com/mtgban/api-gatewahy/session"
	"github.com/mtgban/mtgban-website/apihandoff"
	"github.com/mtgban/mtgban-website/apiproductlist"
)

// testServer wires a Server on the in-memory store with the logging mailer.
type testServer struct {
	*Server
	store *memStore
	mail  *bytes.Buffer
	mux   *http.ServeMux
	now   time.Time
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	store := newMemStore()
	store.clock = func() time.Time { return now }
	var mailBuf bytes.Buffer
	s := &Server{
		Store:             store,
		Catalog:           apiproductlist.MustLoad(),
		Games:             []string{"magic", "pokemon"},
		KnownStores:       nil,
		Sessions:          &session.Codec{Secret: []byte("0123456789abcdef0123456789abcdef"), Now: func() time.Time { return now }},
		Mail:              &mailer.Log{Out: &mailBuf},
		PublicURL:         "https://api.test",
		PricingURL:        "https://mtgban.com/api-plans",
		SuccessPath:       "/checkout/success",
		CancelPath:        "/checkout/cancel",
		AdminEmails:       []string{"admin@example.com"},
		TrialDays:         15,
		GameSecrets:       map[string][]byte{"magic": []byte("trial-secret"), "pokemon": []byte("pokemon-secret")},
		LoginLinksPerHour: 3,
		Now:               func() time.Time { return now },
		Log:               log.New(&bytes.Buffer{}, "", 0),
	}
	mux := http.NewServeMux()
	s.Register(mux)
	return &testServer{Server: s, store: store, mail: &mailBuf, mux: mux, now: now}
}

// do runs one request; cookies carry across when passed in. A POST is marked same-origin.
func (ts *testServer) do(method, target string, form string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	return ts.doOrigin(method, target, form, "https://api.test", cookies...)
}

// doCrossSite is do, but the request claims a foreign Origin, as a forged cross-site POST would.
func (ts *testServer) doCrossSite(method, target string, form string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	return ts.doOrigin(method, target, form, "https://evil.example", cookies...)
}

func (ts *testServer) doOrigin(method, target string, form string, origin string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	var body *strings.Reader
	if form != "" {
		body = strings.NewReader(form)
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, target, body)
	if form != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if method == "POST" {
		req.Header.Set("Origin", origin)
	}
	for _, c := range cookies {
		if c != nil {
			req.AddCookie(c)
		}
	}
	rec := httptest.NewRecorder()
	ts.mux.ServeHTTP(rec, req)
	return rec
}

// signIn creates an account and returns its session cookie and CSRF token.
func (ts *testServer) signIn(t *testing.T, email string) (apiaccess.Account, *http.Cookie, string) {
	t.Helper()
	a, err := ts.store.GetOrCreateAccount(context.Background(), email, "")
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	sess := ts.Sessions.Issue(rec, session.Session{AccountID: a.ID, Email: a.Email})
	return a, rec.Result().Cookies()[0], ts.Sessions.CSRF(sess)
}

// cookieFor seals v into a pending cookie for the test server.
func cookieFor(ts *testServer, v map[string][]string) *http.Cookie {
	rec := httptest.NewRecorder()
	ts.Sessions.SetPending(rec, v, time.Hour)
	return cookieNamed(rec, session.PendingName)
}

func cookieNamed(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func TestHomeRedirectsToPricing(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do("GET", "/", "")
	if rec.Code != 302 || rec.Header().Get("Location") != "https://mtgban.com/api-plans" {
		t.Errorf("%d %q", rec.Code, rec.Header().Get("Location"))
	}
	rec = ts.do("GET", "/static/portal.css", "")
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "text/css") {
		t.Errorf("css %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func TestRenderSetsSecurityHeaders(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do("GET", "/login", "")
	if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options: %q", got)
	}
	if got := rec.Header().Get("Content-Security-Policy"); got != "frame-ancestors 'none'" {
		t.Errorf("Content-Security-Policy: %q", got)
	}
	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy: %q", got)
	}
}

func TestValidReturnTo(t *testing.T) {
	const fb = "https://mtgban.com/api-plans"
	cases := map[string]string{
		"https://pokemon.mtgban.com/api-plans?x=1": "https://pokemon.mtgban.com/api-plans?x=1",
		"https://mtgban.com/":                      "https://mtgban.com/",
		"https://MTGBAN.com/api-plans":             "https://MTGBAN.com/api-plans",
		"http://localhost:8080/api-plans":          "http://localhost:8080/api-plans",
		"https://evil.com/?mtgban.com":             fb,
		"https://notmtgban.com/":                   fb,
		"https://mtgban.com.evil.com/":             fb,
		"http://pokemon.mtgban.com/":               fb,
		"https://user@mtgban.com/":                 fb,
		"javascript:alert(1)":                      fb,
		"/relative":                                fb,
		"":                                         fb,
	}
	for in, want := range cases {
		if got := validReturnTo(in, fb); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
	if got := siteOrigin("https://pokemon.mtgban.com/api-plans?x=1"); got != "https://pokemon.mtgban.com" {
		t.Errorf("origin %q", got)
	}
}

func TestSessionGateRedirectsAndChecksCSRF(t *testing.T) {
	ts := newTestServer(t)
	ts.mux.HandleFunc("GET /gated", ts.withSession(func(w http.ResponseWriter, r *http.Request, sess session.Session, a apiaccess.Account) {
		_, _ = w.Write([]byte("hello " + a.Email))
	}))
	ts.mux.HandleFunc("POST /gated", ts.withSession(func(w http.ResponseWriter, r *http.Request, sess session.Session, a apiaccess.Account) {
		_, _ = w.Write([]byte("posted"))
	}))
	if rec := ts.do("GET", "/gated", ""); rec.Code != 302 || rec.Header().Get("Location") != "/login" {
		t.Errorf("anonymous: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	a, ck, csrf := ts.signIn(t, "ann@example.com")
	if rec := ts.do("GET", "/gated", "", ck); rec.Code != 200 || !strings.Contains(rec.Body.String(), "hello ann@example.com") {
		t.Errorf("signed in: %d %s", rec.Code, rec.Body.String())
	}
	if rec := ts.do("POST", "/gated", "csrf=wrong", ck); rec.Code != 403 {
		t.Errorf("bad csrf: %d", rec.Code)
	}
	if rec := ts.do("POST", "/gated", "csrf="+csrf, ck); rec.Code != 200 {
		t.Errorf("good csrf: %d %s", rec.Code, rec.Body.String())
	}
	if rec := ts.do("POST", "/gated", "csrf="+csrf); rec.Code != 401 {
		t.Errorf("anonymous post: %d", rec.Code)
	}
	_ = ts.store.SetAccountStatus(context.Background(), a.ID, "suspended")
	if rec := ts.do("GET", "/gated", "", ck); rec.Code != 403 || !strings.Contains(rec.Body.String(), "suspended") {
		t.Errorf("suspended: %d %s", rec.Code, rec.Body.String())
	}
}

func TestDescribeEntitlement(t *testing.T) {
	ts := newTestServer(t)
	until := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		e    apiaccess.Entitlement
		want entitlementView
	}{
		{apiaccess.Entitlement{Source: "stripe", Games: []string{"magic"}, StoreScope: "BASE_ACCESS", Modes: []string{"retail", "buylist"}},
			entitlementView{Source: "Stripe subscription", Package: "Base Access", Games: "magic", Stores: "every EU and US store", Modes: "retail, buylist", Until: ""}},
		{apiaccess.Entitlement{Source: "trial", Games: []string{"magic", "pokemon"}, StoreScope: "ALL_ACCESS", Modes: []string{"retail", "buylist", "sealed"}, ValidUntil: &until},
			entitlementView{Source: "Trial", Package: "All Access", Games: "magic, pokemon", Stores: "every store", Modes: "retail, buylist, sealed", Until: "October 5, 2026", Trial: true}},
		{apiaccess.Entitlement{Source: "manual", Games: []string{"magic"}, StoreScope: "TCGLow,TCGMarket,TCGDirect,TCGDirectNet,TCGPlayer,CK,ZZZ", Modes: []string{"retail"}},
			entitlementView{Source: "Arranged with MTGBAN", Package: "À la carte", Games: "magic", Stores: "TCGplayer, Card Kingdom, ZZZ", Modes: "retail"}},
	}
	for _, tc := range cases {
		if got := ts.describeEntitlement(tc.e); got != tc.want {
			t.Errorf("\n got %+v\nwant %+v", got, tc.want)
		}
	}
}

func TestPlanValuesRoundTrip(t *testing.T) {
	v := planValues(planFromValues(map[string][]string{"package": {"starter"}, "interval": {"monthly"}, "games": {"magic,pokemon"}, "stores": {"CK", "SCG"}}))
	if v.Get("package") != "starter" || v.Get("interval") != "monthly" || v.Get("games") != "magic,pokemon" || v.Get("stores") != "CK,SCG" {
		t.Errorf("%v", v)
	}
}

// entitlementFor is an active entitlement for magic with the given source and scope.
func entitlementFor(accountID int64, source, scope string) apiaccess.Entitlement {
	return apiaccess.Entitlement{AccountID: accountID, Source: source, Games: []string{"magic"}, StoreScope: scope,
		Modes: []string{"retail", "buylist"}, Status: "active", ExternalRef: "sub_1"}
}

func TestSameOrigin(t *testing.T) {
	ts := newTestServer(t)
	cases := []struct {
		name     string
		secFetch string
		origin   string
		referer  string
		want     bool
	}{
		{"same-origin passes with no Origin", "same-origin", "", "", true},
		{"cross-site fails even with a matching Origin", "cross-site", "https://api.test", "", false},
		{"none with matching Origin passes", "none", "https://api.test", "", true},
		{"none with no headers fails", "none", "", "", false},
		{"matching Origin in different case passes", "", "HTTPS://API.TEST", "", true},
		{"Referer under the public URL passes", "", "", "https://api.test/login/x", true},
		{"Referer on another host fails", "", "", "https://evil.example/", false},
	}
	for _, tc := range cases {
		req := httptest.NewRequest("POST", "/x", nil)
		if tc.secFetch != "" {
			req.Header.Set("Sec-Fetch-Site", tc.secFetch)
		}
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		if tc.referer != "" {
			req.Header.Set("Referer", tc.referer)
		}
		if got := ts.sameOrigin(req); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestTokenPostsRequireSameOrigin(t *testing.T) {
	ts := newTestServer(t)
	ts.do("POST", "/login", "email=ann%40example.com")
	m := linkRe.FindStringSubmatch(ts.mail.String())
	if m == nil {
		t.Fatalf("no link mailed:\n%s", ts.mail.String())
	}
	if rec := ts.doCrossSite("POST", "/login/"+m[1], ""); rec.Code != 403 {
		t.Fatalf("cross-site login: %d", rec.Code)
	}
	if rec := ts.do("POST", "/login/"+m[1], ""); rec.Code != 302 {
		t.Fatalf("same-origin login after refusal: %d", rec.Code)
	}

	tok := ts.handoff(apihandoff.PurposeTrial, "trialorigin@example.com")
	if rec := ts.doCrossSite("POST", "/trial", "t="+tok); rec.Code != 403 {
		t.Fatalf("cross-site trial: %d", rec.Code)
	}
	if _, err := ts.store.GetAccountByEmail(context.Background(), "trialorigin@example.com"); err == nil {
		t.Error("cross-site trial created an account")
	}
	if rec := ts.do("POST", "/trial", "t="+tok); rec.Code != 302 {
		t.Fatalf("same-origin trial after refusal: %d", rec.Code)
	}
}

func TestReservedPaths(t *testing.T) {
	for _, p := range []string{"/login", "/account/keys", "/admin/accounts/1", "/static/portal.css", "/checkout", "/healthz", "/stripe/webhook", "/v1/games.json"} {
		if !Reserved(p) {
			t.Errorf("%q: want reserved", p)
		}
	}
	for _, p := range []string{"/checkout/success", "/thanks"} {
		if Reserved(p) {
			t.Errorf("%q: want not reserved", p)
		}
	}
}

func TestMemStoreConsumeNonce(t *testing.T) {
	m := newMemStore()
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	if err := m.ConsumeNonce(context.Background(), "n", now.Add(time.Hour), now); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := m.ConsumeNonce(context.Background(), "n", now.Add(time.Hour), now); !errors.Is(err, apiaccess.ErrNonceUsed) {
		t.Fatalf("second: %v", err)
	}
	if err := m.ConsumeNonce(context.Background(), "n", now.Add(3*time.Hour), now.Add(2*time.Hour)); err != nil {
		t.Fatalf("after sweep: %v", err)
	}
}
