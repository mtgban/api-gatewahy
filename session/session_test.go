package session

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func codecAt(now time.Time) *Codec {
	return &Codec{Secret: []byte("0123456789abcdef0123456789abcdef"), Now: func() time.Time { return now }}
}

func TestSessionRoundTrip(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	c := codecAt(now)
	rec := httptest.NewRecorder()
	issued := c.Issue(rec, Session{AccountID: 7, Email: "ann@example.com"})
	if issued.IssuedAt != now || issued.ExpiresAt != now.Add(DefaultTTL) {
		t.Errorf("stamps %+v", issued)
	}
	ck := rec.Result().Cookies()[0]
	if ck.Name != CookieName || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode || ck.MaxAge != int(DefaultTTL.Seconds()) {
		t.Errorf("cookie %+v", ck)
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(ck)
	got, err := c.Read(req)
	if err != nil || got != issued {
		t.Fatalf("read %+v %v want %+v", got, err, issued)
	}
	late := codecAt(now.Add(DefaultTTL))
	if _, err := late.Read(req); !errors.Is(err, ErrInvalid) {
		t.Errorf("expired: %v", err)
	}
	other := &Codec{Secret: []byte("another-secret-another-secret-12"), Now: c.Now}
	if _, err := other.Read(req); !errors.Is(err, ErrInvalid) {
		t.Errorf("wrong secret: %v", err)
	}
	tampered := ck.Value[:len(ck.Value)-2] + "zz"
	if _, err := c.Decode(tampered); !errors.Is(err, ErrInvalid) {
		t.Errorf("tampered: %v", err)
	}
	if _, err := c.Read(httptest.NewRequest("GET", "/", nil)); !errors.Is(err, ErrInvalid) {
		t.Errorf("no cookie: %v", err)
	}
}

func TestCSRFIsBoundToTheSession(t *testing.T) {
	c := codecAt(time.Now())
	a := Session{AccountID: 1, IssuedAt: time.Unix(100, 0)}
	b := Session{AccountID: 2, IssuedAt: time.Unix(100, 0)}
	if !c.CheckCSRF(a, c.CSRF(a)) || c.CheckCSRF(a, c.CSRF(b)) || c.CheckCSRF(a, "") {
		t.Error("csrf check wrong")
	}
}

func TestPendingValues(t *testing.T) {
	now := time.Now()
	c := codecAt(now)
	rec := httptest.NewRecorder()
	c.SetPending(rec, url.Values{"package": {"starter"}, "games": {"magic", "pokemon"}}, time.Hour)
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(rec.Result().Cookies()[0])
	v, err := c.Pending(req)
	if err != nil || v.Get("package") != "starter" || len(v["games"]) != 2 || v.Get("_exp") == "" {
		t.Fatalf("pending %v %v", v, err)
	}
	if _, err := codecAt(now.Add(2 * time.Hour)).Pending(req); !errors.Is(err, ErrInvalid) {
		t.Errorf("expired pending: %v", err)
	}
	rec = httptest.NewRecorder()
	c.Clear(rec)
	c.ClearPending(rec)
	for _, ck := range rec.Result().Cookies() {
		if ck.MaxAge >= 0 || !strings.Contains(CookieName+PendingName, ck.Name) {
			t.Errorf("clear cookie %+v", ck)
		}
	}
}
