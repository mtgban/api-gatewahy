// Package session is the portal's signed cookie: who is signed in, a sealed
// pending checkout, and a CSRF token derived from the session. Nothing is
// stored server side; rotating the secret signs everyone out.
package session

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Cookie names, host-only on the gateway so they never mix with the website's MTGBAN cookie.
const (
	CookieName  = "ban_session"
	PendingName = "ban_pending"
)

// DefaultTTL is how long a session lasts.
const DefaultTTL = 30 * 24 * time.Hour

// ErrInvalid covers a missing, forged, malformed, or expired token alike.
var ErrInvalid = errors.New("session: invalid or expired")

// Session is who is signed in.
type Session struct {
	AccountID int64
	Email     string
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// Codec signs and reads sessions and other sealed values.
type Codec struct {
	Secret []byte
	TTL    time.Duration
	// Secure is false only for plain-http local runs
	Secure bool
	Now    func() time.Time
}

func (c *Codec) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Codec) ttl() time.Duration {
	if c.TTL > 0 {
		return c.TTL
	}
	return DefaultTTL
}

// Seal signs v with an expiry of ttl from now under the reserved key _exp.
func (c *Codec) Seal(v url.Values, ttl time.Duration) string {
	out := url.Values{}
	for k, vals := range v {
		out[k] = vals
	}
	out.Set("_exp", strconv.FormatInt(c.now().Add(ttl).Unix(), 10))
	body := base64.RawURLEncoding.EncodeToString([]byte(out.Encode()))
	return body + "." + c.sign(body)
}

// Open verifies a sealed token and returns its values, _exp included.
func (c *Codec) Open(token string) (url.Values, error) {
	body, sig, ok := strings.Cut(token, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(c.sign(body))) {
		return nil, ErrInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, ErrInvalid
	}
	v, err := url.ParseQuery(string(raw))
	if err != nil {
		return nil, ErrInvalid
	}
	exp, err := strconv.ParseInt(v.Get("_exp"), 10, 64)
	if err != nil || !c.now().Before(time.Unix(exp, 0)) {
		return nil, ErrInvalid
	}
	return v, nil
}

func (c *Codec) sign(body string) string {
	mac := hmac.New(sha256.New, c.Secret)
	mac.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Encode signs s, stamping IssuedAt and ExpiresAt from now and TTL.
func (c *Codec) Encode(s Session) (string, Session) {
	s.IssuedAt = c.now().Truncate(time.Second)
	s.ExpiresAt = s.IssuedAt.Add(c.ttl())
	v := url.Values{}
	v.Set("a", strconv.FormatInt(s.AccountID, 10))
	v.Set("e", s.Email)
	v.Set("iat", strconv.FormatInt(s.IssuedAt.Unix(), 10))
	return c.Seal(v, c.ttl()), s
}

// Decode reads a token Encode produced.
func (c *Codec) Decode(token string) (Session, error) {
	v, err := c.Open(token)
	if err != nil {
		return Session{}, err
	}
	id, err1 := strconv.ParseInt(v.Get("a"), 10, 64)
	iat, err2 := strconv.ParseInt(v.Get("iat"), 10, 64)
	exp, _ := strconv.ParseInt(v.Get("_exp"), 10, 64)
	if err1 != nil || err2 != nil || id <= 0 || v.Get("e") == "" {
		return Session{}, ErrInvalid
	}
	return Session{AccountID: id, Email: v.Get("e"), IssuedAt: time.Unix(iat, 0).UTC(), ExpiresAt: time.Unix(exp, 0).UTC()}, nil
}

// Issue sets the session cookie and returns the session as stamped.
func (c *Codec) Issue(w http.ResponseWriter, s Session) Session {
	token, s := c.Encode(s)
	http.SetCookie(w, c.cookie(CookieName, token, int(c.ttl().Seconds())))
	return s
}

// Read returns the session in r's cookie.
func (c *Codec) Read(r *http.Request) (Session, error) {
	ck, err := r.Cookie(CookieName)
	if err != nil {
		return Session{}, ErrInvalid
	}
	return c.Decode(ck.Value)
}

// Clear expires the session cookie.
func (c *Codec) Clear(w http.ResponseWriter) {
	http.SetCookie(w, c.cookie(CookieName, "", -1))
}

// SetPending seals v into the pending cookie for ttl.
func (c *Codec) SetPending(w http.ResponseWriter, v url.Values, ttl time.Duration) {
	http.SetCookie(w, c.cookie(PendingName, c.Seal(v, ttl), int(ttl.Seconds())))
}

// Pending reads the pending cookie.
func (c *Codec) Pending(r *http.Request) (url.Values, error) {
	ck, err := r.Cookie(PendingName)
	if err != nil {
		return nil, ErrInvalid
	}
	return c.Open(ck.Value)
}

// ClearPending expires the pending cookie.
func (c *Codec) ClearPending(w http.ResponseWriter) {
	http.SetCookie(w, c.cookie(PendingName, "", -1))
}

func (c *Codec) cookie(name, value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true, Secure: c.Secure, SameSite: http.SameSiteLaxMode}
}

// CSRF is the form token for s, an HMAC over the session identity so it needs no storage.
func (c *Codec) CSRF(s Session) string {
	return c.sign("csrf|" + strconv.FormatInt(s.AccountID, 10) + "|" + strconv.FormatInt(s.IssuedAt.Unix(), 10))
}

// CheckCSRF reports whether token is s's CSRF token.
func (c *Codec) CheckCSRF(s Session, token string) bool {
	return token != "" && hmac.Equal([]byte(token), []byte(c.CSRF(s)))
}
