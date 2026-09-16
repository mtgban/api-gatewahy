package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"sync"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/api-gatewahy/config"
	"github.com/mtgban/api-gatewahy/discord"
	"github.com/mtgban/api-gatewahy/gateway"
	"github.com/mtgban/mtgban-website/observability"
)

func init() {
	commands["serve"] = command{usage: "run the gateway", run: serve}
}

func serve(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "config path or b2:// URL (default $BAN_CONFIG_PATH)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	log.SetOutput(stderr)

	cfg, err := config.Load(ctx, *configPath)
	if err != nil {
		fmt.Fprintln(stderr, "api-gatewahy:", err)
		return 1
	}
	store, err := apiaccess.NewClient(*cfg.APIAccess)
	if err != nil {
		fmt.Fprintln(stderr, "api-gatewahy:", err)
		return 1
	}
	defer func() { _ = store.Close() }()

	var events gateway.EventSink
	if cfg.Observability != nil {
		oc, err := observability.NewClient(*cfg.Observability)
		if err != nil {
			log.Println("observability disabled:", err)
		} else {
			rec := observability.NewRecorder(oc)
			// The recorder must flush before its client's pool goes away.
			defer func() { _ = oc.Close() }()
			defer func() { _ = rec.Close() }()
			events = rec
		}
	}

	srv, cleanup, err := newServer(cfg, store, events, discord.New(cfg.DiscordHook))
	if err != nil {
		fmt.Fprintln(stderr, "api-gatewahy:", err)
		return 1
	}
	defer cleanup()

	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		fmt.Fprintln(stderr, "api-gatewahy:", err)
		return 1
	}
	log.Printf("api-gatewahy %s listening on %s for %v", version, srv.Addr, cfg.GameNames())
	grace := time.Duration(cfg.ShutdownGraceSeconds) * time.Second
	if err := serveUntilDone(ctx, srv, ln, grace); err != nil {
		fmt.Fprintln(stderr, "api-gatewahy:", err)
		return 1
	}
	return 0
}

// serveUntilDone serves ln until ctx ends, then returns once Shutdown has
// drained the in-flight requests or the grace period expires.
func serveUntilDone(ctx context.Context, srv *http.Server, ln net.Listener, grace time.Duration) error {
	serverFailed := make(chan struct{})
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		select {
		case <-ctx.Done():
		case <-serverFailed:
			return
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); errors.Is(err, context.DeadlineExceeded) {
			log.Printf("warning: shutdown grace of %s expired with requests still in flight", grace)
		}
	}()

	// Serve returns ErrServerClosed as soon as Shutdown starts, so wait it out.
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		<-shutdownDone
		return nil
	}
	close(serverFailed)
	return err
}

// newServer wires the handler, listener, jobs, and mux. cleanup stops them.
func newServer(cfg *config.Config, store *apiaccess.Client, events gateway.EventSink, poster *discord.Poster) (*http.Server, func(), error) {
	games := map[string]gateway.Upstream{}
	for name, g := range cfg.Games {
		u, err := url.Parse(g.Upstream)
		if err != nil {
			return nil, nil, fmt.Errorf("games.%s: %w", name, err)
		}
		games[name] = gateway.Upstream{URL: u, Secret: []byte(g.Secret)}
	}

	resolver := gateway.NewResolver(store, time.Duration(cfg.CacheTTLSeconds)*time.Second, nil)
	resolver.SetStaleGrace(time.Duration(cfg.StaleGraceSeconds) * time.Second)
	meter := gateway.NewUsageMeter(store, events, cfg.InstanceName, 5*time.Second, 200)
	handler := gateway.New(gateway.Options{
		Games:           games,
		GatewayEmail:    cfg.GatewayEmail,
		Link:            cfg.Link,
		ClientIPHeader:  cfg.ClientIPHeader,
		PerKeyRate:      cfg.PerKeyRequestsPerSec,
		PerKeyBurst:     cfg.PerKeyBurst,
		UpstreamTimeout: time.Duration(cfg.UpstreamTimeoutSeconds) * time.Second,
		SigTTL:          5 * time.Minute,
	}, resolver, meter)

	listener, err := apiaccess.Listen(cfg.APIAccess.DSN(),
		func(hash string) { resolver.Invalidate(hash) },
		func() { resolver.Invalidate("") },
		func(err error) { log.Println("reload listener:", err) })
	if err != nil {
		_ = meter.Close()
		return nil, nil, fmt.Errorf("reload listener: %w", err)
	}

	jobsCtx, stopJobs := context.WithCancel(context.Background())
	alert := func(msg string) {
		if err := poster.Post(jobsCtx, msg); err != nil {
			log.Println("discord:", err)
		}
	}
	prober := gateway.NewProber(games, cfg.GatewayEmail, cfg.Link, nil, alert)
	var jobs sync.WaitGroup
	jobs.Add(2)
	go func() {
		defer jobs.Done()
		prober.Run(jobsCtx, time.Hour)
	}()
	go func() {
		defer jobs.Done()
		var lastDropped int64
		runDaily(jobsCtx, 0, 5, func(ctx context.Context, now time.Time) {
			dropped := meter.Dropped()
			dailySummary(ctx, store, alert, now, cfg.UsageRetentionDays, dropped-lastDropped)
			lastDropped = dropped
		})
	}()

	mux := newMux(muxDeps{
		games:   handler.GameNames(),
		healthy: store.PingContext,
		gateway: handler,
	})
	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	cleanup := func() {
		stopJobs()
		jobs.Wait()
		_ = listener.Close()
		_ = meter.Close()
	}
	return srv, cleanup, nil
}

type muxDeps struct {
	games   []string
	healthy func(context.Context) error
	gateway http.Handler
}

func newMux(d muxDeps) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := d.healthy(ctx); err != nil || len(d.games) == 0 {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/v1/games.json", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = w.Write([]byte(`{"error": "method not allowed"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(d.games)
	})
	mux.Handle("/v1/", d.gateway)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error": "not found"}`))
	})
	return recoverPanics(mux)
}

// wroteWriter remembers whether the handler sent anything yet.
type wroteWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *wroteWriter) WriteHeader(code int) {
	w.wrote = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *wroteWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the real writer.
func (w *wroteWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// recoverPanics turns a handler panic into a JSON 500. An aborted response is
// re-panicked so the server still tears the connection down and metering runs.
func recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ww := &wroteWriter{ResponseWriter: w}
		defer func() {
			p := recover()
			if p == nil {
				return
			}
			if p == http.ErrAbortHandler {
				panic(p)
			}
			log.Printf("panic serving %s: %v\n%s", r.URL.Path, p, debug.Stack())
			if !ww.wrote {
				ww.Header().Set("Content-Type", "application/json")
				ww.WriteHeader(http.StatusInternalServerError)
				_, _ = ww.Write([]byte(`{"error": "internal error"}`))
			}
		}()
		next.ServeHTTP(ww, r)
	})
}

// nextRunAt is the next hh:mm UTC strictly after now.
func nextRunAt(now time.Time, hour, minute int) time.Time {
	now = now.UTC()
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, time.UTC)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// runDaily calls fn at hh:mm UTC every day until ctx ends.
func runDaily(ctx context.Context, hour, minute int, fn func(context.Context, time.Time)) {
	for {
		timer := time.NewTimer(time.Until(nextRunAt(time.Now(), hour, minute)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			fn(ctx, time.Now())
		}
	}
}

// dailySummary posts yesterday's usage. dropped counts rows lost since the last summary.
func dailySummary(ctx context.Context, store *apiaccess.Client, alert func(string), now time.Time, retentionDays int, dropped int64) {
	day := now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
	rows, err := store.SummarizeUsage(ctx, day, day.AddDate(0, 0, 1), 0)
	if err != nil {
		log.Println("daily summary:", err)
		return
	}
	keys, err := store.KeysCreatedSince(ctx, day)
	if err != nil {
		log.Println("daily summary keys:", err)
	}
	alert(gateway.SummaryText(day, rows, keys, dropped))
	if n, err := store.PruneUsage(ctx, now.AddDate(0, 0, -retentionDays)); err != nil {
		log.Println("prune usage:", err)
	} else if n > 0 {
		log.Printf("pruned %d usage rows", n)
	}
}
