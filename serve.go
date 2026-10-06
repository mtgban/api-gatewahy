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
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/api-gatewahy/billing"
	"github.com/mtgban/api-gatewahy/config"
	"github.com/mtgban/api-gatewahy/discord"
	"github.com/mtgban/api-gatewahy/gateway"
	"github.com/mtgban/api-gatewahy/mailer"
	"github.com/mtgban/api-gatewahy/portal"
	"github.com/mtgban/api-gatewahy/session"
	"github.com/mtgban/mtgban-website/apiproductlist"
	"github.com/mtgban/mtgban-website/observability"
)

func init() {
	commands["serve"] = command{usage: "run the gateway", run: serve}
}

// stripeDeps is what serve needs when STRIPE_SECRET_KEY is set.
type stripeDeps struct {
	api           billing.API
	webhookSecret string
}

// stripeDepsFromEnv reads the two Stripe secrets. Neither set means billing is off.
func stripeDepsFromEnv() (*stripeDeps, error) {
	if os.Getenv("STRIPE_SECRET_KEY") == "" {
		return nil, nil
	}
	api, err := stripeFromEnv()
	if err != nil {
		return nil, err
	}
	secret := os.Getenv("STRIPE_WEBHOOK_SECRET")
	if secret == "" {
		return nil, errors.New("STRIPE_WEBHOOK_SECRET is required when STRIPE_SECRET_KEY is set")
	}
	return &stripeDeps{api: api, webhookSecret: secret}, nil
}

// portalDeps is what serve needs when GATEWAY_SESSION_SECRET is set.
type portalDeps struct {
	sessionSecret []byte
	mail          mailer.Mailer
}

// portalDepsFromEnv reads the portal secrets. No session secret means the portal is off.
func portalDepsFromEnv(cfg *config.Config, stderr io.Writer) (*portalDeps, error) {
	secret := os.Getenv("GATEWAY_SESSION_SECRET")
	if secret == "" {
		return nil, nil
	}
	d := &portalDeps{sessionSecret: []byte(secret)}
	smtp, err := mailer.FromEnv(cfg.Mail.From)
	if err != nil {
		return nil, err
	}
	if smtp == nil {
		log.Println("MAIL_SMTP_HOST not set: mail goes to the log, sign-in links included")
		d.mail = &mailer.Log{Out: stderr}
	} else {
		d.mail = smtp
	}
	return d, nil
}

// portalCodec builds the portal's session codec, rejecting a short secret.
func portalCodec(pd *portalDeps, cfg *config.Config) (*session.Codec, error) {
	codec, err := session.NewCodec(pd.sessionSecret, 0, strings.HasPrefix(cfg.PublicURL, "https://"), nil)
	if err != nil {
		return nil, fmt.Errorf("GATEWAY_SESSION_SECRET: %w", err)
	}
	return codec, nil
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
	store, err := apiaccess.NewClient(ctx, *cfg.APIAccess)
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

	sd, err := stripeDepsFromEnv()
	if err != nil {
		fmt.Fprintln(stderr, "api-gatewahy:", err)
		return 1
	}
	if sd == nil {
		log.Println("stripe disabled: STRIPE_SECRET_KEY not set")
	}

	pd, err := portalDepsFromEnv(cfg, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "api-gatewahy:", err)
		return 1
	}
	if pd == nil {
		log.Println("portal disabled: GATEWAY_SESSION_SECRET not set")
	}
	srv, cleanup, err := newServer(cfg, store, events, discord.New(cfg.DiscordHook), sd, pd)
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
func newServer(cfg *config.Config, store *apiaccess.Client, events gateway.EventSink, poster *discord.Poster, sd *stripeDeps, pd *portalDeps) (*http.Server, func(), error) {
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
	resolver.SetLookupTimeout(time.Duration(cfg.LookupTimeoutSeconds) * time.Second)
	meter := gateway.NewUsageMeter(store, events, cfg.InstanceName, 5*time.Second, 200)
	sigTTL := time.Duration(cfg.SigTTLSeconds) * time.Second
	handler, err := gateway.New(gateway.Options{
		Games:           games,
		GatewayEmail:    cfg.GatewayEmail,
		Link:            cfg.Link,
		ClientIPHeader:  cfg.ClientIPHeader,
		PerKeyRate:      cfg.PerKeyRequestsPerSec,
		PerKeyBurst:     cfg.PerKeyBurst,
		PerIPRate:       cfg.PerIPRequestsPerSec,
		PerIPBurst:      cfg.PerIPBurst,
		UpstreamTimeout: time.Duration(cfg.UpstreamTimeoutSeconds) * time.Second,
		SigTTL:          sigTTL,
	}, resolver, meter)
	if err != nil {
		_ = meter.Close()
		return nil, nil, fmt.Errorf("gateway options: %w", err)
	}

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
	prober := gateway.NewProber(games, cfg.GatewayEmail, cfg.Link, sigTTL, nil, nil, store.PingContext, alert)
	var jobs sync.WaitGroup
	jobs.Add(2)
	go func() {
		defer jobs.Done()
		prober.Run(jobsCtx, time.Hour)
	}()
	go func() {
		defer jobs.Done()
		var lastDropped int64
		runDaily(jobsCtx, 0, 5, "daily summary", alert, func(ctx context.Context, now time.Time) {
			dropped := meter.Dropped()
			dailySummary(ctx, store, alert, now, cfg, dropped-lastDropped)
			lastDropped = dropped
		})
	}()

	abort := func() {
		stopJobs()
		jobs.Wait()
		_ = listener.Close()
		_ = meter.Close()
	}

	var (
		cat     *apiproductlist.ProductList
		stores  billing.StoreLister
		rec     *billing.Reconciler
		webhook http.Handler
		web     *portal.Server
	)
	if sd != nil || pd != nil {
		cat = apiproductlist.MustLoad()
		stores = billing.NewSiteStoreClient(cfg, alert)
	}
	if sd != nil {
		rec = newReconciler(store, sd.api, cfg, cat, stores, alert)
		webhook = &billing.Webhook{Secret: sd.webhookSecret, Ledger: store, Reconcile: rec.Subscription}
		jobs.Add(1)
		go func() {
			defer jobs.Done()
			runDaily(jobsCtx, 3, 0, "stripe reconcile", alert, func(ctx context.Context, _ time.Time) {
				res, err := rec.All(ctx)
				if err != nil {
					alert("api-gatewahy: stripe reconcile failed: " + err.Error())
					return
				}
				alert(res.Summary())
			})
		}()
	}
	if pd != nil {
		if err := checkReservedPaths(cfg); err != nil {
			abort()
			return nil, nil, err
		}
		codec, err := portalCodec(pd, cfg)
		if err != nil {
			abort()
			return nil, nil, err
		}
		web = &portal.Server{
			Store: store, Catalog: cat, Games: cfg.GameNames(), Stores: stores,
			Sessions:          codec,
			Mail:              pd.mail,
			PublicURL:         strings.TrimRight(cfg.PublicURL, "/"),
			PricingURL:        cfg.PricingURL,
			SuccessPath:       cfg.Stripe.SuccessPath,
			CancelPath:        cfg.Stripe.CancelPath,
			AdminEmails:       cfg.AdminEmails,
			TrialDays:         cfg.TrialDays,
			GameSecrets:       gameSecrets(cfg),
			LoginLinksPerHour: cfg.LoginLinksPerHour,
			ClientIPHeader:    cfg.ClientIPHeader,
		}
		if sd != nil {
			web.Stripe = sd.api
			web.Checkout = newCheckout(store, sd.api, cfg, cat, stores)
			web.Reconcile = rec.Subscription
			web.ReconcileAll = rec.All
		}
		jobs.Add(1)
		go func() {
			defer jobs.Done()
			runDaily(jobsCtx, 9, 0, "trial reminders", alert, web.SendTrialReminders)
		}()
	}

	mux := newMux(muxDeps{
		games:       handler.GameNames(),
		gateway:     handler,
		webhook:     webhook,
		portal:      web,
		successPath: cfg.Stripe.SuccessPath,
		cancelPath:  cfg.Stripe.CancelPath,
	})
	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// A request body has 30s; a response may stream a full snapshot, so it gets the upstream budget plus slack.
		ReadTimeout:  30 * time.Second,
		WriteTimeout: time.Duration(cfg.UpstreamTimeoutSeconds)*time.Second + 30*time.Second,
		IdleTimeout:  120 * time.Second,
	}
	return srv, abort, nil
}

// gameSecrets is each configured game's shared secret, the one the gateway
// calls the site with and the site signs handoff tokens with.
func gameSecrets(cfg *config.Config) map[string][]byte {
	out := make(map[string][]byte, len(cfg.Games))
	for name, g := range cfg.Games {
		out[name] = []byte(g.Secret)
	}
	return out
}

// checkReservedPaths refuses a Stripe landing path that collides with a portal route.
func checkReservedPaths(cfg *config.Config) error {
	if portal.Reserved(cfg.Stripe.SuccessPath) {
		return fmt.Errorf("stripe.success_path %q collides with a portal route", cfg.Stripe.SuccessPath)
	}
	if portal.Reserved(cfg.Stripe.CancelPath) {
		return fmt.Errorf("stripe.cancel_path %q collides with a portal route", cfg.Stripe.CancelPath)
	}
	return nil
}

type muxDeps struct {
	games       []string
	gateway     http.Handler
	webhook     http.Handler
	portal      *portal.Server
	successPath string
	cancelPath  string
}

func newMux(d muxDeps) http.Handler {
	mux := http.NewServeMux()
	// Liveness only: answers from the process alone, with no DB ping, so a
	// DB blip cannot get the container restarted. The prober reports DB health.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if len(d.games) == 0 {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/v1/games.json", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(d.games)
	})
	if d.webhook != nil {
		mux.Handle("/stripe/webhook", d.webhook)
	}
	switch {
	case d.portal != nil:
		d.portal.Register(mux)
	case d.webhook != nil:
		mux.HandleFunc(d.successPath, plainPage("Payment received. Your access is being set up and your key will work within a minute. You can close this page."))
		mux.HandleFunc(d.cancelPath, plainPage("Checkout cancelled. Nothing was charged. You can close this page."))
	}
	mux.Handle("/v1/", d.gateway)
	mux.Handle("/v2/", d.gateway)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSONError(w, http.StatusNotFound, "not found")
	})
	return recoverPanics(mux)
}

// writeJSONError writes the gateway's uniform JSON error shape.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// msg is a plain ASCII constant, so %q is valid JSON.
	_, _ = w.Write([]byte(fmt.Sprintf(`{"error": %q}`, msg)))
}

// plainPage serves one sentence as text/plain to a GET.
func plainPage(text string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(text + "\n"))
	}
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
				writeJSONError(ww, http.StatusInternalServerError, "internal error")
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
func runDaily(ctx context.Context, hour, minute int, name string, alert func(string), fn func(context.Context, time.Time)) {
	runOn(ctx, func(now time.Time) time.Time { return nextRunAt(now, hour, minute) }, name, alert, fn)
}

// runOn calls fn each time next(time.Now()) elapses, until ctx ends.
func runOn(ctx context.Context, next func(time.Time) time.Time, name string, alert func(string), fn func(context.Context, time.Time)) {
	for {
		timer := time.NewTimer(time.Until(next(time.Now())))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			runJob(ctx, name, alert, fn, time.Now())
		}
	}
}

// maxAlertPanicLen caps the panic value in the alert text; the log keeps it in full.
const maxAlertPanicLen = 300

// runJob runs one invocation of a named job, recovering and alerting a panic
// so the caller's loop keeps going.
func runJob(ctx context.Context, name string, alert func(string), fn func(context.Context, time.Time), now time.Time) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("panic in job %s: %v\n%s", name, p, debug.Stack())
			func() {
				// A panic from alert must not take the job loop down too.
				defer func() { _ = recover() }()
				alert(fmt.Sprintf("api-gatewahy: job %s panicked: %s", name, capPanicText(p)))
			}()
		}
	}()
	fn(ctx, now)
}

// capPanicText truncates a panic value for the alert text.
func capPanicText(p any) string {
	s := fmt.Sprint(p)
	if len(s) > maxAlertPanicLen {
		return s[:maxAlertPanicLen] + "..."
	}
	return s
}

// Retention of the tables the daily job prunes without a config key.
const (
	stripeEventRetentionDays = 30 // by received_at; Stripe retries an event for 3 days
	inviteRetentionDays      = 30 // past expires_at, used or not
)

// dailyStore is what the daily summary reads and prunes.
type dailyStore interface {
	SummarizeUsage(ctx context.Context, since, until time.Time, accountID int64) ([]apiaccess.UsageRow, error)
	KeysCreatedBetween(ctx context.Context, from, to time.Time) ([]apiaccess.Key, error)
	PruneUsage(ctx context.Context, before time.Time) (int64, error)
	PruneStripeEvents(ctx context.Context, before time.Time) (int64, error)
	PruneInvites(ctx context.Context, before time.Time) (int64, error)
	PruneAdminActions(ctx context.Context, before time.Time) (int64, error)
}

// dailySummary posts yesterday's usage, then prunes. dropped counts rows lost since the last summary.
func dailySummary(ctx context.Context, store dailyStore, alert func(string), now time.Time, cfg *config.Config, dropped int64) {
	postSummary(ctx, store, alert, now, dropped)
	pruneTables(ctx, store, now, cfg)
}

// postSummary alerts yesterday's usage, or logs why it cannot.
func postSummary(ctx context.Context, store dailyStore, alert func(string), now time.Time, dropped int64) {
	day := now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
	rows, err := store.SummarizeUsage(ctx, day, day.AddDate(0, 0, 1), 0)
	if err != nil {
		log.Println("daily summary:", err)
		return
	}
	keys, err := store.KeysCreatedBetween(ctx, day, day.AddDate(0, 0, 1))
	if err != nil {
		log.Println("daily summary keys:", err)
	}
	alert(summaryText(day, rows, keys, dropped))
}

// humanBytes renders n with a binary unit suffix.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// summaryText renders the daily Discord message.
func summaryText(day time.Time, rows []apiaccess.UsageRow, newKeys []apiaccess.Key, dropped int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "API usage for %s\n", day.Format("2006-01-02"))
	if len(rows) == 0 {
		b.WriteString("no API traffic\n")
	}
	for _, r := range rows {
		fmt.Fprintf(&b, "%s / %s: %d requests, %s, %d errors\n", r.Email, r.Game, r.Requests, humanBytes(r.Bytes), r.Errors)
	}
	for _, k := range newKeys {
		fmt.Fprintf(&b, "new key %s (%s) for account %d\n", k.Prefix, k.Label, k.AccountID)
	}
	if dropped > 0 {
		fmt.Fprintf(&b, "%d usage rows dropped\n", dropped)
	}
	return b.String()
}

// pruneTables deletes rows past each table's retention, logging each count.
func pruneTables(ctx context.Context, store dailyStore, now time.Time, cfg *config.Config) {
	for _, p := range []struct {
		what  string
		days  int
		prune func(context.Context, time.Time) (int64, error)
	}{
		{"usage rows", cfg.UsageRetentionDays, store.PruneUsage},
		{"stripe events", stripeEventRetentionDays, store.PruneStripeEvents},
		{"invites", inviteRetentionDays, store.PruneInvites},
		{"admin actions", cfg.AuditRetentionDays, store.PruneAdminActions},
	} {
		// Zero keeps the table forever; only admin_actions_retention_days can be zero.
		if p.days == 0 {
			continue
		}
		if n, err := p.prune(ctx, now.AddDate(0, 0, -p.days)); err != nil {
			log.Printf("prune %s: %v", p.what, err)
		} else if n > 0 {
			log.Printf("pruned %d %s", n, p.what)
		}
	}
}
