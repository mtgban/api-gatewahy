package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/mtgban-website/apisig"
	"github.com/mtgban/mtgban-website/ratelimit"
	"golang.org/x/time/rate"
)

// Upstream is one game backend.
type Upstream struct {
	URL    *url.URL
	Secret []byte
}

// Meter receives one Usage per proxied request. Record must not block.
type Meter interface {
	Record(u apiaccess.Usage)
}

// Options configure a Handler.
type Options struct {
	Games          map[string]Upstream
	GatewayEmail   string
	Link           string
	ClientIPHeader string
	// PerKeyRate and PerKeyBurst throttle per account, so extra keys on one account share the allowance.
	PerKeyRate  float64
	PerKeyBurst int
	// PerIPRate and PerIPBurst throttle by client address before any key is looked up.
	PerIPRate       float64
	PerIPBurst      int
	UpstreamTimeout time.Duration
	SigTTL          time.Duration
	Now             func() time.Time
}

// Handler is the /v1/ gateway.
type Handler struct {
	opts    Options
	games   map[string]Upstream
	proxies map[string]*httputil.ReverseProxy
	res     *Resolver
	meter   Meter
	limiter *accountLimiter
	// ipLimiter runs first, so forged keys cannot drive the database.
	ipLimiter *ratelimit.Limiter
	now       func() time.Time
}

// accountLimiter throttles per account and remembers its rate for the response header.
type accountLimiter struct {
	*ratelimit.Limiter
	perSec int
}

func newLimiter(perSec float64, burst int) *accountLimiter {
	return &accountLimiter{Limiter: ratelimit.NewLimiter(rate.Limit(perSec), burst), perSec: int(perSec)}
}

// New builds a handler with one reverse proxy per game.
func New(opts Options, res *Resolver, meter Meter) *Handler {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.SigTTL == 0 {
		opts.SigTTL = 5 * time.Minute
	}
	if opts.PerIPRate <= 0 {
		opts.PerIPRate = 50
	}
	if opts.PerIPBurst <= 0 {
		opts.PerIPBurst = 100
	}
	h := &Handler{
		opts:      opts,
		games:     opts.Games,
		proxies:   map[string]*httputil.ReverseProxy{},
		res:       res,
		meter:     meter,
		limiter:   newLimiter(opts.PerKeyRate, opts.PerKeyBurst),
		ipLimiter: ratelimit.NewLimiter(rate.Limit(opts.PerIPRate), opts.PerIPBurst),
		now:       opts.Now,
	}
	for name, up := range opts.Games {
		h.proxies[name] = h.newProxy(name, up)
	}
	return h
}

// GameNames lists configured games sorted.
func (h *Handler) GameNames() []string {
	names := make([]string, 0, len(h.games))
	for n := range h.games {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

type ctxKey struct{}

// proxyParams travel from ServeHTTP to the proxy's Rewrite through the context.
type proxyParams struct {
	sig      string
	path     string
	clientIP string
	game     string
}

// newProxy builds the reverse proxy for one game. The game name travels per request.
func (h *Handler) newProxy(_ string, up Upstream) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: h.opts.UpstreamTimeout,
			MaxIdleConnsPerHost:   16,
		},
		Rewrite: func(pr *httputil.ProxyRequest) {
			p, _ := pr.In.Context().Value(ctxKey{}).(proxyParams)
			pr.SetURL(up.URL)
			pr.Out.URL.Path = p.path
			pr.Out.URL.RawPath = ""
			q := pr.In.URL.Query()
			q.Del("sig")
			q.Del("key")
			q.Set("sig", p.sig)
			pr.Out.URL.RawQuery = q.Encode()
			pr.Out.Host = up.URL.Host
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("X-Real-Ip")
			pr.Out.Header.Del("Forwarded")
			if h.opts.ClientIPHeader != "" {
				pr.Out.Header.Del(h.opts.ClientIPHeader)
			}
			// The backends' rate limiter reads X-Forwarded-For, so send the resolved address.
			if p.clientIP != "" {
				pr.Out.Header.Set("X-Forwarded-For", p.clientIP)
			} else {
				pr.Out.Header.Del("X-Forwarded-For")
			}
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
		},
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusNotModified {
				return nil
			}
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return errUpstreamStatus{resp.StatusCode}
			}
			// The backends answer a bad signature with 200 and a body of
			// {"error": "invalid signature"} or "invalid or expired signature".
			if resp.StatusCode == http.StatusOK && strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
				head := make([]byte, 300)
				n, _ := io.ReadFull(resp.Body, head)
				head = head[:n]
				resp.Body = struct {
					io.Reader
					io.Closer
				}{io.MultiReader(bytes.NewReader(head), resp.Body), resp.Body}
				if sigRejected(head) {
					return errSigRejected{}
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			p, _ := r.Context().Value(ctxKey{}).(proxyParams)
			var status errUpstreamStatus
			var netErr net.Error
			switch {
			case errors.As(err, &errSigRejected{}):
				log.Printf("gateway: %s: backend rejected gateway signature", p.game)
				writeError(w, http.StatusBadGateway, "upstream rejected gateway signature", p.game)
			case errors.As(err, &status):
				log.Printf("gateway: %s: upstream returned %d for %s", p.game, status.code, p.path)
				writeError(w, http.StatusBadGateway, status.Error(), p.game)
			case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
				log.Printf("gateway: %s: upstream timed out for %s", p.game, p.path)
				writeError(w, http.StatusGatewayTimeout, "upstream timed out", p.game)
			default:
				// Log the error class only; a transport error can carry the minted sig.
				log.Printf("gateway: %s: upstream unavailable for %s: %T", p.game, p.path, err)
				writeError(w, http.StatusBadGateway, "upstream unavailable", p.game)
			}
		},
	}
}

// sigRejected reports whether a 200 body is the backends' signature error.
// That error is an object whose first key is "error" with a value starting
// "invalid", so the check is on the shape, not on a substring anywhere in
// the body, which price data could carry.
func sigRejected(head []byte) bool {
	head = bytes.TrimSpace(head)
	for _, prefix := range []string{`{"error": "invalid`, `{"error":"invalid`} {
		if bytes.HasPrefix(head, []byte(prefix)) {
			return true
		}
	}
	return false
}

// bearerKey reads the key from the Authorization header or ?key=.
func bearerKey(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); auth != "" {
		if k, ok := strings.CutPrefix(auth, "Bearer "); ok {
			return strings.TrimSpace(k)
		}
		return auth
	}
	return r.URL.Query().Get("key")
}

// ClientIP is the address in header, else the peer address. A multi-valued
// header yields its last element, the one the trusted edge appended; anything
// before it came from the client. Anything that is not an IP literal, or a
// zoned IPv6 literal, falls back to the peer.
func ClientIP(r *http.Request, header string) string {
	if header != "" {
		if v := r.Header.Get(header); v != "" {
			parts := strings.Split(v, ",")
			if ip, err := netip.ParseAddr(strings.TrimSpace(parts[len(parts)-1])); err == nil && ip.Zone() == "" {
				return ip.Unmap().String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.Zone() != "" {
		return ""
	}
	return ip.Unmap().String()
}

// statusWriter records what the proxy wrote.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the real writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "")
		return
	}
	route, err := ParseRoute(r.URL.Path)
	if err != nil {
		writeError(w, http.StatusNotFound, "not found", "")
		return
	}
	up, ok := h.games[route.Game]
	// h.games aliases the caller's map, so a game can outlive its proxy.
	proxy := h.proxies[route.Game]
	if !ok || proxy == nil {
		writeError(w, http.StatusNotFound, "unknown game", route.Game)
		return
	}

	ip := ClientIP(r, h.opts.ClientIPHeader)
	// Throttled by address before any key is read, so forged keys cannot drive the database.
	if !h.ipLimiter.Allow(ip) {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "too many requests from this address", "")
		return
	}

	plain := bearerKey(r)
	if plain == "" {
		writeError(w, http.StatusUnauthorized, "missing API key", "")
		return
	}
	if !apiaccess.LooksLikeKey(plain) {
		writeError(w, http.StatusUnauthorized, "malformed API key", "")
		return
	}
	hash := apiaccess.HashKey(plain)
	lk, err := h.res.Resolve(r.Context(), hash)
	switch {
	case errors.Is(err, ErrUnknownKey):
		writeError(w, http.StatusUnauthorized, "unknown API key", "")
		return
	case err != nil:
		w.Header().Set("Retry-After", "30")
		writeError(w, http.StatusServiceUnavailable, "key lookup unavailable, try again shortly", "")
		return
	}
	if lk.Key.RevokedAt != nil {
		writeError(w, http.StatusUnauthorized, "revoked API key", "")
		return
	}
	if lk.Account.Status != "active" {
		writeError(w, http.StatusUnauthorized, "account suspended", "")
		return
	}

	now := h.now()
	access, ok := Resolve(lk.Entitlements, route.Game, now)
	if !ok {
		writeError(w, http.StatusForbidden, "plan does not include game", route.Game)
		return
	}
	// DEV_ACCESS is never issued, so a plan carrying it must not mint a signature.
	if access.StoreScope == "" || access.StoreScope == "DEV_ACCESS" {
		writeError(w, http.StatusForbidden, "plan has no store scope", route.Game)
		return
	}
	for _, m := range route.NeedsModes() {
		if !access.HasMode(m) {
			writeError(w, http.StatusForbidden, "plan does not include mode "+m, route.Game)
			return
		}
	}

	w.Header().Set("RateLimit-Limit", strconv.Itoa(h.limiter.perSec))
	if !h.limiter.Allow("account:" + strconv.FormatInt(lk.Account.ID, 10)) {
		writeError(w, http.StatusTooManyRequests, "too many requests", "")
		return
	}

	sig := apisig.Mint(up.Secret, h.opts.Link, apisig.Claims{
		API: access.StoreScope,
		// The field names are apisig.APIFields.
		Fields: url.Values{
			"APImode":   {strings.Join(access.Modes, ",")},
			"UserEmail": {h.opts.GatewayEmail},
		},
		Expires: now.Add(h.opts.SigTTL).Unix(),
	})

	params := proxyParams{sig: sig, path: route.BackendPath(), clientIP: ip, game: route.Game}
	ctx, cancel := context.WithTimeout(context.WithValue(r.Context(), ctxKey{}, params), h.opts.UpstreamTimeout)
	defer cancel()

	w.Header().Set("X-MTGBAN-Game", route.Game)
	w.Header().Set("X-MTGBAN-Account", strconv.FormatInt(lk.Account.ID, 10))

	sw := &statusWriter{ResponseWriter: w}
	start := time.Now()
	// Deferred: ReverseProxy panics with ErrAbortHandler when a copy dies mid-stream.
	defer func() {
		status := sw.status
		if status == 0 {
			// Nothing was ever written: the client went away first.
			status = 499
		}
		h.meter.Record(apiaccess.Usage{
			Ts:         start,
			KeyID:      lk.Key.ID,
			AccountID:  lk.Account.ID,
			Game:       route.Game,
			Path:       route.BackendPath(),
			Status:     status,
			Bytes:      sw.bytes,
			DurationMS: int(time.Since(start) / time.Millisecond),
			ClientIP:   ip,
		})
	}()
	proxy.ServeHTTP(sw, r.WithContext(ctx))
}
