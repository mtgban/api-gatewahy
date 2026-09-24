package portal

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/mtgban/api-gatewahy/session"
)

var linkRe = regexp.MustCompile(`https://api\.test/login/(\S+)`)

func TestLoginByMagicLink(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do("GET", "/login", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `name="email"`) || !strings.Contains(rec.Body.String(), "https://mtgban.com/api-login") {
		t.Fatalf("form: %d %s", rec.Code, rec.Body.String())
	}

	rec = ts.do("POST", "/login", "email=Ann%40Example.com")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Check your inbox") {
		t.Fatalf("post: %d %s", rec.Code, rec.Body.String())
	}
	m := linkRe.FindStringSubmatch(ts.mail.String())
	if m == nil {
		t.Fatalf("no link mailed:\n%s", ts.mail.String())
	}
	a, err := ts.store.GetAccountByEmail(context.Background(), "ann@example.com")
	if err != nil {
		t.Fatal("account not created")
	}

	rec = ts.do("GET", "/login/"+m[1], "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `action="/login/`+m[1]+`"`) {
		t.Fatalf("confirm: %d %s", rec.Code, rec.Body.String())
	}

	rec = ts.do("POST", "/login/"+m[1], "")
	ck := cookieNamed(rec, session.CookieName)
	if rec.Code != 302 || rec.Header().Get("Location") != "/account?notice=login" || ck == nil {
		t.Fatalf("token: %d %q cookie %v", rec.Code, rec.Header().Get("Location"), ck)
	}
	sess, err := ts.Sessions.Decode(ck.Value)
	if err != nil || sess.AccountID != a.ID || sess.Email != "ann@example.com" {
		t.Errorf("session %+v %v", sess, err)
	}

	rec = ts.do("POST", "/login/"+m[1], "")
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "invalid or has expired") {
		t.Errorf("reuse: %d", rec.Code)
	}
	if rec := ts.do("POST", "/login/nonsense", ""); rec.Code != 400 {
		t.Errorf("unknown token: %d", rec.Code)
	}
	if rec := ts.do("GET", "/login/nonsense", ""); rec.Code != 200 {
		t.Errorf("unknown token confirm: %d", rec.Code)
	}

	// The response for an existing account is the same page.
	ts.mail.Reset()
	rec = ts.do("POST", "/login", "email=ann%40example.com")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Check your inbox") {
		t.Errorf("existing: %d", rec.Code)
	}
}

func TestLoginRejectsBadEmailAndRateLimits(t *testing.T) {
	ts := newTestServer(t)
	if rec := ts.do("POST", "/login", "email=not-an-email"); rec.Code != 400 {
		t.Errorf("bad email: %d", rec.Code)
	}
	if rec := ts.do("POST", "/login", "email="+strings.Repeat("a", 300)+"%40example.com"); rec.Code != 400 {
		t.Errorf("long email: %d", rec.Code)
	}
	if ts.mail.Len() != 0 {
		t.Error("long email sent mail")
	}
	for i := 0; i < 3; i++ {
		if rec := ts.do("POST", "/login", "email=ann%40example.com"); rec.Code != 200 {
			t.Fatalf("request %d: %d", i, rec.Code)
		}
	}
	if rec := ts.do("POST", "/login", "email=ann%40example.com"); rec.Code != 429 {
		t.Errorf("fourth: %d", rec.Code)
	}
	// Same IP, different email: the per-IP limit is already spent.
	if rec := ts.do("POST", "/login", "email=bob%40example.com"); rec.Code != 429 {
		t.Errorf("per ip: %d", rec.Code)
	}
}

func TestLoginRequiresSameOrigin(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.doCrossSite("POST", "/login", "email=ann%40example.com")
	if rec.Code != 403 {
		t.Errorf("cross-site: %d", rec.Code)
	}
	if ts.mail.Len() != 0 {
		t.Error("cross-site login sent mail")
	}
	rec = ts.do("POST", "/login", "email=ann%40example.com")
	if rec.Code != 200 {
		t.Errorf("same-origin: %d", rec.Code)
	}
}

func TestLoginContinuesPendingCheckout(t *testing.T) {
	ts := newTestServer(t)
	ts.do("POST", "/login", "email=ann%40example.com")
	m := linkRe.FindStringSubmatch(ts.mail.String())
	pending := cookieFor(ts, map[string][]string{"package": {"starter"}, "interval": {"monthly"}, "games": {"magic"}, "stores": {"CK"}, "return_to": {"https://mtgban.com/api-plans"}})
	rec := ts.do("POST", "/login/"+m[1], "", pending)
	if rec.Code != 302 || rec.Header().Get("Location") != "/checkout" {
		t.Errorf("%d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestSuspendedAccountCannotSignIn(t *testing.T) {
	ts := newTestServer(t)
	a, _ := ts.store.GetOrCreateAccount(context.Background(), "ann@example.com", "")
	_ = ts.store.SetAccountStatus(context.Background(), a.ID, "suspended")
	ts.do("POST", "/login", "email=ann%40example.com")
	m := linkRe.FindStringSubmatch(ts.mail.String())
	rec := ts.do("POST", "/login/"+m[1], "")
	if rec.Code != 403 || cookieNamed(rec, session.CookieName) != nil {
		t.Errorf("%d", rec.Code)
	}
}

func TestLogoutClearsCookies(t *testing.T) {
	ts := newTestServer(t)
	_, ck, csrf := ts.signIn(t, "ann@example.com")
	rec := ts.do("POST", "/logout", "csrf="+csrf, ck)
	if rec.Code != 302 || rec.Header().Get("Location") != "https://mtgban.com/api-plans" {
		t.Errorf("%d %q", rec.Code, rec.Header().Get("Location"))
	}
	if c := cookieNamed(rec, session.CookieName); c == nil || c.MaxAge >= 0 {
		t.Error("session cookie not cleared")
	}
	// The old cookie is dead server-side too: the logout bumped the account epoch.
	if rec := ts.do("GET", "/account", "", ck); rec.Code != 302 {
		t.Errorf("stale cookie still signed in: %d", rec.Code)
	}

	b, ck2, csrf2 := ts.signIn(t, "bob@example.com")
	_ = ts.store.SetAccountStatus(context.Background(), b.ID, "suspended")
	rec = ts.do("POST", "/logout", "csrf="+csrf2, ck2)
	if rec.Code != 302 || rec.Header().Get("Location") != "https://mtgban.com/api-plans" {
		t.Errorf("suspended: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if c := cookieNamed(rec, session.CookieName); c == nil || c.MaxAge >= 0 {
		t.Error("suspended: session cookie not cleared")
	}
}
