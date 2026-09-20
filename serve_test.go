package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mtgban/api-gatewahy/config"
	"github.com/mtgban/api-gatewahy/mailer"
	"github.com/mtgban/api-gatewahy/portal"
	"github.com/mtgban/api-gatewahy/session"
	"github.com/mtgban/mtgban-website/apiproductlist"
)

var errDown = errors.New("down")

func TestNextRunAt(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	got := nextRunAt(now, 0, 5)
	want := time.Date(2026, 9, 16, 0, 5, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v want %v", got, want)
	}
	early := time.Date(2026, 9, 15, 0, 1, 0, 0, time.UTC)
	if got := nextRunAt(early, 0, 5); !got.Equal(time.Date(2026, 9, 15, 0, 5, 0, 0, time.UTC)) {
		t.Errorf("got %v", got)
	}
}

func TestMuxGamesAndHealth(t *testing.T) {
	mux := newMux(muxDeps{
		games:   []string{"magic", "pokemon"},
		healthy: func(context.Context) error { return nil },
		gateway: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(299) }),
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/games.json", nil))
	var games []string
	if err := json.Unmarshal(rec.Body.Bytes(), &games); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || len(games) != 2 || games[0] != "magic" {
		t.Errorf("games: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/games.json", nil))
	if rec.Code != 405 || rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("games post: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Errorf("healthz %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/magic/mtgban/retail.json", nil))
	if rec.Code != 299 {
		t.Errorf("gateway not reached: %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/somewhere", nil))
	if rec.Code != 404 || rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("fallthrough %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func TestHealthzUnhealthy(t *testing.T) {
	mux := newMux(muxDeps{games: []string{"magic"}, healthy: func(context.Context) error { return errDown }, gateway: http.NotFoundHandler()})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 503 {
		t.Errorf("healthz %d", rec.Code)
	}
}

// TestServeWaitsForShutdown covers the drain that keeps cleanup from cutting
// off in-flight requests when the serve context is cancelled.
func TestServeWaitsForShutdown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var handlerDone atomic.Bool
	srv := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(300 * time.Millisecond)
			handlerDone.Store(true)
			w.WriteHeader(http.StatusOK)
		}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type result struct {
		code int
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/slow")
		if err != nil {
			got <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
		got <- result{code: resp.StatusCode}
	}()
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	if err := serveUntilDone(ctx, srv, ln, 5*time.Second); err != nil {
		t.Fatalf("serveUntilDone: %v", err)
	}
	if !handlerDone.Load() {
		t.Error("serveUntilDone returned before the handler finished")
	}
	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("request: %v", r.err)
		}
		if r.code != http.StatusOK {
			t.Errorf("request got %d want 200", r.code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request never completed")
	}
}

// TestServeUntilDoneReturnsServeError checks a listen failure is reported and
// does not park the shutdown goroutine forever.
func TestServeUntilDoneReturnsServeError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.NotFoundHandler()}
	if err := serveUntilDone(context.Background(), srv, ln, 5*time.Second); err == nil {
		t.Error("want an error from a closed listener")
	}
}

func TestMuxRecoversPanic(t *testing.T) {
	mux := newMux(muxDeps{
		games:   []string{"magic"},
		healthy: func(context.Context) error { return nil },
		gateway: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic("boom") }),
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/magic/mtgban/retail.json", nil))
	if rec.Code != 500 || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d headers %v", rec.Code, rec.Header())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "internal error" {
		t.Errorf("body %v", body)
	}
}

func TestMuxPropagatesAbortHandler(t *testing.T) {
	mux := newMux(muxDeps{
		games:   []string{"magic"},
		healthy: func(context.Context) error { return nil },
		gateway: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic(http.ErrAbortHandler) }),
	})
	defer func() {
		if p := recover(); p != http.ErrAbortHandler {
			t.Errorf("recovered %v, want http.ErrAbortHandler", p)
		}
	}()
	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/magic/mtgban/retail.json", nil))
	t.Error("the panic did not propagate")
}

func TestHealthzPingTimeout(t *testing.T) {
	mux := newMux(muxDeps{
		games: []string{"magic"},
		healthy: func(ctx context.Context) error {
			if _, ok := ctx.Deadline(); !ok {
				return errors.New("no deadline on the health check")
			}
			return nil
		},
		gateway: http.NotFoundHandler(),
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Errorf("healthz %d %s", rec.Code, rec.Body.String())
	}
}

func TestMuxWebhookAndCheckoutPages(t *testing.T) {
	webhookHit := false
	mux := newMux(muxDeps{
		games:       []string{"magic"},
		healthy:     func(context.Context) error { return nil },
		gateway:     http.NotFoundHandler(),
		webhook:     http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { webhookHit = true; w.WriteHeader(http.StatusOK) }),
		successPath: "/checkout/success",
		cancelPath:  "/checkout/cancel",
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/stripe/webhook", nil))
	if rec.Code != 200 || !webhookHit {
		t.Errorf("webhook not reached: %d", rec.Code)
	}
	for _, path := range []string{"/checkout/success", "/checkout/cancel"} {
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") || rec.Body.Len() == 0 {
			t.Errorf("%s: %d %q", path, rec.Code, rec.Header().Get("Content-Type"))
		}
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("POST", path, nil))
		if rec.Code != 405 {
			t.Errorf("%s POST: %d", path, rec.Code)
		}
	}
}

func TestMuxWithoutBilling(t *testing.T) {
	mux := newMux(muxDeps{games: []string{"magic"}, healthy: func(context.Context) error { return nil }, gateway: http.NotFoundHandler()})
	for _, path := range []string{"/stripe/webhook", "/checkout/success"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("POST", path, nil))
		if rec.Code != 404 || rec.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s without billing: %d %q", path, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
}

func TestCheckCatalogStores(t *testing.T) {
	cat := &apiproductlist.ProductList{Stores: []apiproductlist.Store{
		{Key: "CK", Shorthands: []string{"CK"}},
		{Key: "SCG", Shorthands: []string{"SCG", "StarCityGames"}},
	}}
	if err := checkCatalogStores(cat, nil); err != nil {
		t.Errorf("empty known: %v", err)
	}
	if err := checkCatalogStores(cat, []string{"CK", "SCG", "StarCityGames"}); err != nil {
		t.Errorf("every shorthand known: %v", err)
	}
	err := checkCatalogStores(cat, []string{"CK", "SCG"})
	if err == nil || !strings.Contains(err.Error(), `"StarCityGames"`) {
		t.Errorf("missing shorthand: %v", err)
	}
}

func TestMuxMountsPortal(t *testing.T) {
	web := &portal.Server{
		Catalog: apiproductlist.MustLoad(), Games: []string{"magic"},
		Sessions:   &session.Codec{Secret: []byte("0123456789abcdef0123456789abcdef")},
		PricingURL: "https://mtgban.com/api-plans", SuccessPath: "/checkout/success", CancelPath: "/checkout/cancel",
	}
	// The real gateway handler answers an unmatched /v1/ path with a JSON 404
	// (gateway/handler.go's writeError); http.NotFoundHandler here would give
	// a false failure on the last assertion below, so mimic that shape.
	jsonNotFound := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error": "not found"}`))
	})
	mux := newMux(muxDeps{games: []string{"magic"}, healthy: func(context.Context) error { return nil }, gateway: jsonNotFound,
		portal: web, successPath: "/checkout/success", cancelPath: "/checkout/cancel"})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 302 || rec.Header().Get("Location") != "https://mtgban.com/api-plans" {
		t.Errorf("root: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/login", nil))
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
		t.Errorf("login: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/nope.json", nil))
	if rec.Code != 404 || rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("api fallthrough still json: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func TestPortalDepsFromEnv(t *testing.T) {
	cfg := &config.Config{Mail: config.MailConfig{From: "MTGBAN <no-reply@mtgban.com>"}}
	t.Setenv("GATEWAY_SESSION_SECRET", "")
	if d, err := portalDepsFromEnv(cfg, io.Discard); d != nil || err != nil {
		t.Errorf("off: %v %v", d, err)
	}
	t.Setenv("GATEWAY_SESSION_SECRET", "short")
	t.Setenv("TRIAL_SECRET", "t")
	if _, err := portalDepsFromEnv(cfg, io.Discard); err == nil {
		t.Error("short secret accepted")
	}
	t.Setenv("GATEWAY_SESSION_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("TRIAL_SECRET", "")
	if _, err := portalDepsFromEnv(cfg, io.Discard); err == nil {
		t.Error("missing TRIAL_SECRET accepted")
	}
	t.Setenv("TRIAL_SECRET", "t")
	t.Setenv("MAIL_SMTP_HOST", "")
	d, err := portalDepsFromEnv(cfg, io.Discard)
	if err != nil || d == nil {
		t.Fatalf("%v %v", d, err)
	}
	if _, ok := d.mail.(*mailer.Log); !ok {
		t.Errorf("mail %T, want the logging mailer", d.mail)
	}
}

func TestStripeDepsFromEnv(t *testing.T) {
	t.Setenv("STRIPE_SECRET_KEY", "")
	t.Setenv("STRIPE_WEBHOOK_SECRET", "")
	if sd, err := stripeDepsFromEnv(); sd != nil || err != nil {
		t.Errorf("unset: %v %v", sd, err)
	}
	t.Setenv("STRIPE_SECRET_KEY", "sk_test_x")
	if _, err := stripeDepsFromEnv(); err == nil || !strings.Contains(err.Error(), "STRIPE_WEBHOOK_SECRET") {
		t.Errorf("missing webhook secret: %v", err)
	}
	t.Setenv("STRIPE_WEBHOOK_SECRET", "whsec_x")
	sd, err := stripeDepsFromEnv()
	if err != nil || sd == nil || sd.api == nil || sd.webhookSecret != "whsec_x" {
		t.Errorf("both set: %+v %v", sd, err)
	}
}
