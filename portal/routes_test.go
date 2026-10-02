package portal

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// samplePath fills a route pattern's wildcard segments with a placeholder,
// to build a concrete request path from it.
func samplePath(pattern string) string {
	if pattern == "/{$}" {
		return "/"
	}
	var b strings.Builder
	for i := 0; i < len(pattern); i++ {
		if pattern[i] == '{' {
			if j := strings.IndexByte(pattern[i:], '}'); j >= 0 {
				b.WriteString("1")
				i += j
				continue
			}
		}
		b.WriteByte(pattern[i])
	}
	return b.String()
}

// doSecFetchCrossSite sends a request carrying Sec-Fetch-Site: cross-site,
// as a forged cross-site POST would.
func (ts *testServer) doSecFetchCrossSite(method, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(""))
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	ts.mux.ServeHTTP(rec, req)
	return rec
}

// TestRouteTableNoPublicPOST catches a new POST route that forgets a guard:
// every POST must be guardSameOrigin, guardSession, guardAdmin, or guardCSRFIfSignedIn.
func TestRouteTableNoPublicPOST(t *testing.T) {
	ts := newTestServer(t)
	for _, rt := range ts.routes() {
		if rt.method == "POST" && rt.guard == guardPublic {
			t.Errorf("%s %s: guardPublic on a POST route", rt.method, rt.pattern)
		}
	}
}

// TestRouteTableInlineOnlySameOrigin keeps inline from silently exempting a
// guardSession or guardAdmin route from every session assertion below.
func TestRouteTableInlineOnlySameOrigin(t *testing.T) {
	ts := newTestServer(t)
	for _, rt := range ts.routes() {
		if rt.inline && rt.guard != guardSameOrigin {
			t.Errorf("%s %s: inline on a non-guardSameOrigin route", rt.method, rt.pattern)
		}
	}
}

// TestRouteTableSessionGuards checks every guardSession/guardAdmin route's
// refusal: 401 with no session, 403 with a session but no CSRF, redirect on GET.
func TestRouteTableSessionGuards(t *testing.T) {
	ts := newTestServer(t)
	_, ck, _ := ts.signIn(t, "guardtest@example.com")
	for _, rt := range ts.routes() {
		if rt.guard != guardSession && rt.guard != guardAdmin {
			continue
		}
		path := samplePath(rt.pattern)
		switch rt.method {
		case "POST":
			rec := ts.do("POST", path, "")
			if rec.Code != 401 || !strings.Contains(rec.Body.String(), "sign in first") {
				t.Errorf("%s %s: no session: got %d %q", rt.method, rt.pattern, rec.Code, rec.Body.String())
			}
			rec = ts.do("POST", path, "", ck)
			if rec.Code != 403 || !strings.Contains(rec.Body.String(), "this form expired, go back and try again") {
				t.Errorf("%s %s: session, no csrf: got %d %q", rt.method, rt.pattern, rec.Code, rec.Body.String())
			}
		case "GET":
			if rec := ts.do("GET", path, ""); rec.Code != 302 || rec.Header().Get("Location") != "/login" {
				t.Errorf("%s %s: no session: got %d %q", rt.method, rt.pattern, rec.Code, rec.Header().Get("Location"))
			}
		}
	}
}

// TestRouteTableCSRFIfSignedIn walks every guardCSRFIfSignedIn route
// (logout): 403 for a session with no CSRF, 302 for no session at all.
func TestRouteTableCSRFIfSignedIn(t *testing.T) {
	ts := newTestServer(t)
	_, ck, _ := ts.signIn(t, "csrfifsignedin@example.com")
	for _, rt := range ts.routes() {
		if rt.guard != guardCSRFIfSignedIn {
			continue
		}
		path := samplePath(rt.pattern)
		rec := ts.do(rt.method, path, "", ck)
		if rec.Code != 403 || !strings.Contains(rec.Body.String(), "this form expired, go back and try again") {
			t.Errorf("%s %s: session, no csrf: got %d %q", rt.method, rt.pattern, rec.Code, rec.Body.String())
		}
		if rec := ts.do(rt.method, path, ""); rec.Code != 302 {
			t.Errorf("%s %s: no session: got %d want 302", rt.method, rt.pattern, rec.Code)
		}
	}
}

// TestRouteTableSameOrigin checks every guardSameOrigin route: a cross-site
// POST is refused with 403 and the message the inline preamble used to show.
func TestRouteTableSameOrigin(t *testing.T) {
	ts := newTestServer(t)
	const sharedMsg = "That request did not come from this site. Open the link again and use the button on the page."
	const loginMsg = "That request did not come from this site. Use the form on this page."
	for _, rt := range ts.routes() {
		if rt.guard != guardSameOrigin {
			continue
		}
		want := sharedMsg
		if rt.inline {
			want = loginMsg
		}
		path := samplePath(rt.pattern)
		rec := ts.doSecFetchCrossSite(rt.method, path)
		if rec.Code != 403 || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s %s: cross-site: got %d %q", rt.method, rt.pattern, rec.Code, rec.Body.String())
		}
	}
}

// TestRouteTableReserved independently re-derives each family's prefix by
// string-splitting the pattern, not by calling routes.go's own derivation.
func TestRouteTableReserved(t *testing.T) {
	ts := newTestServer(t)
	seen := map[string]bool{}
	for _, rt := range ts.routes() {
		if rt.pattern == ts.SuccessPath || rt.pattern == ts.CancelPath {
			continue
		}
		if p := samplePath(rt.pattern); !Reserved(p) {
			t.Errorf("%s %s: %q not reserved", rt.method, rt.pattern, p)
		}
		parts := strings.Split(strings.Trim(rt.pattern, "/"), "/")
		if len(parts) < 2 || seen[parts[0]] {
			continue
		}
		seen[parts[0]] = true
		sibling := "/" + parts[0] + "/zzz-not-a-real-route"
		if !Reserved(sibling) {
			t.Errorf("%s %s: sibling %q of its family not reserved", rt.method, rt.pattern, sibling)
		}
	}
}
