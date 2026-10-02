package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/api-gatewahy/billing"
	"github.com/mtgban/api-gatewahy/config"
	"github.com/mtgban/mtgban-website/apiproductlist"
)

// billingVerbs is the verb table of the billing commands.
var billingVerbs = map[string][]verb[billingDeps]{
	"catalog":  {{"seed", catalogSeed}},
	"checkout": {{"link", checkoutLink}},
	"invite":   {{"create", inviteCreate}},
	"stripe":   {{"reconcile", stripeReconcile}},
	"portal":   {{"link", portalLink}},
	"plan":     {{"change", planChange}},
}

func init() {
	for name, verbs := range billingVerbs {
		commands[name] = command{usage: verbList(verbs), run: billingCommand(name)}
	}
}

// billingCommand opens the store and Stripe and runs one billing command.
func billingCommand(name string) func(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return func(ctx context.Context, args []string, stdout, stderr io.Writer) int {
		return withStore(ctx, args, stderr, func(store *apiaccess.Client, cfg *config.Config, rest []string) int {
			api, err := stripeFromEnv()
			if err != nil {
				return fail(stderr, err)
			}
			return runBilling(ctx, billingDeps{store: store, api: api, cfg: cfg, cat: apiproductlist.MustLoad(), stores: billing.NewSiteStoreClient(cfg, stderrAlert(stderr))}, name, rest, stdout, stderr)
		})
	}
}

// billingDeps is what the billing verbs need; tests leave store and api nil
// for the paths that fail before reaching them.
type billingDeps struct {
	store  billingStore
	api    billing.API
	cfg    *config.Config
	cat    *apiproductlist.ProductList
	stores billing.StoreLister
}

// inviteCreator mints an invite token for a non-public interval.
type inviteCreator interface {
	CreateInvite(ctx context.Context, intervalKey, email string, ttl time.Duration, note string) (string, apiaccess.Invite, error)
}

// billingStore is billing.Store plus the account, grant and invite calls the
// billing verbs make directly, outside the billing package.
type billingStore interface {
	billing.Store
	accountLookup
	entitlementLister
	inviteCreator
}

// stripeFromEnv builds the Stripe client; the key never lives in the config file.
func stripeFromEnv() (billing.API, error) {
	key := os.Getenv("STRIPE_SECRET_KEY")
	if key == "" {
		return nil, errors.New("STRIPE_SECRET_KEY is not set")
	}
	return billing.NewClient(key), nil
}

// stderrAlert prints billing alerts on stderr, where the operator sees them.
func stderrAlert(stderr io.Writer) func(string) {
	return func(msg string) { fmt.Fprintln(stderr, "alert:", msg) }
}

func newCheckout(store billing.Store, api billing.API, cfg *config.Config, cat *apiproductlist.ProductList, stores billing.StoreLister) *billing.Checkout {
	success, cancel := cfg.CheckoutURLs()
	return &billing.Checkout{Store: store, API: api, Catalog: cat, Stores: stores, Games: cfg.GameNames(), SuccessURL: success, CancelURL: cancel}
}

func newReconciler(store billing.Store, api billing.API, cfg *config.Config, cat *apiproductlist.ProductList, stores billing.StoreLister, alert func(string)) *billing.Reconciler {
	return &billing.Reconciler{Store: store, API: api, Catalog: cat, Stores: stores, Grace: time.Duration(cfg.Stripe.GraceDays) * 24 * time.Hour, Alert: alert}
}

// runBilling dispatches one billing command. Exit codes: 0 ok, 1 error, 2 usage.
func runBilling(ctx context.Context, d billingDeps, cmd string, args []string, stdout, stderr io.Writer) int {
	return runVerb(ctx, cmd, billingVerbs[cmd], d, args, stdout, stderr)
}

func catalogSeed(ctx context.Context, d billingDeps, args []string, stdout, stderr io.Writer) int {
	fs := verbFlags("catalog seed", stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	res, err := billing.Seed(ctx, d.api, d.cat)
	if err != nil {
		return fail(stderr, err)
	}
	if len(res.Created)+len(res.Updated)+len(res.Archived) == 0 {
		fmt.Fprintln(stdout, "Stripe already matches the catalog")
		return 0
	}
	for _, id := range res.Created {
		fmt.Fprintln(stdout, "created", id)
	}
	for _, id := range res.Updated {
		fmt.Fprintln(stdout, "updated", id)
	}
	for _, id := range res.Archived {
		fmt.Fprintln(stdout, "archived", id)
	}
	return 0
}

func checkoutLink(ctx context.Context, d billingDeps, args []string, stdout, stderr io.Writer) int {
	fs := verbFlags("checkout link", stderr)
	email := fs.String("email", "", "account email")
	pkg := fs.String("package", "", "package key from the catalog")
	games := fs.String("games", "magic", "comma-separated games")
	stores := fs.String("stores", "", "comma-separated store family keys, starter only")
	interval := fs.String("interval", "monthly", "interval key")
	invite := fs.String("invite", "", "invite token for a non-public interval")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !need(stderr, "email", *email) || !need(stderr, "package", *pkg) {
		return 2
	}
	a, ok := byEmail(ctx, d.store, *email, stderr)
	if !ok {
		return 1
	}
	plan := billing.Plan{Package: *pkg, Interval: *interval, Games: splitList(*games), Stores: splitList(*stores)}
	sess, err := newCheckout(d.store, d.api, d.cfg, d.cat, d.stores).Create(ctx, billing.Request{Account: a, Plan: plan, Invite: *invite})
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "checkout link for %s, valid 24 hours:\n\n    %s\n\n", a.Email, sess.URL)
	return 0
}

func inviteCreate(ctx context.Context, d billingDeps, args []string, stdout, stderr io.Writer) int {
	fs := verbFlags("invite create", stderr)
	interval := fs.String("interval", "monthly", "interval key")
	email := fs.String("email", "", "account email")
	days := fs.Int("days", 14, "invite lifetime in days")
	note := fs.String("note", "", "free-text note")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !need(stderr, "interval", *interval) {
		return 2
	}
	iv, ok := d.cat.Interval(*interval)
	if !ok {
		return fail(stderr, fmt.Errorf("unknown interval %q", *interval))
	}
	if iv.Public {
		return fail(stderr, fmt.Errorf("interval %s is public and does not need an invite", iv.Key))
	}
	token, inv, err := d.store.CreateInvite(ctx, iv.Key, *email, time.Duration(*days)*24*time.Hour, *note)
	if err != nil {
		return fail(stderr, err)
	}
	bound := "any account"
	if inv.Email != "" {
		bound = inv.Email
	}
	fmt.Fprintf(stdout, "invite for %s (%s), expires %s. Shown once, copy it now:\n\n    %s\n\n", iv.Key, bound, inv.ExpiresAt.Format("2006-01-02"), token)
	return 0
}

func stripeReconcile(ctx context.Context, d billingDeps, args []string, stdout, stderr io.Writer) int {
	fs := verbFlags("stripe reconcile", stderr)
	sub := fs.String("sub", "", "subscription id")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rec := newReconciler(d.store, d.api, d.cfg, d.cat, d.stores, stderrAlert(stderr))
	if *sub != "" {
		if err := rec.Subscription(ctx, *sub); err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintf(stdout, "subscription %s reconciled\n", *sub)
		return 0
	}
	res, err := rec.All(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintln(stdout, res.Summary())
	if res.Failed > 0 {
		return 1
	}
	return 0
}

func portalLink(ctx context.Context, d billingDeps, args []string, stdout, stderr io.Writer) int {
	fs := verbFlags("portal link", stderr)
	email := fs.String("email", "", "account email")
	ret := fs.String("return", "", "portal return URL (default public_url)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	a, ok := byEmail(ctx, d.store, *email, stderr)
	if !ok {
		return exitFor(*email)
	}
	returnURL := *ret
	if returnURL == "" {
		returnURL = d.cfg.PublicURL
	}
	url, err := billing.PortalURL(ctx, d.api, a, returnURL)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "portal link for %s:\n\n    %s\n\n", a.Email, url)
	return 0
}

// planChange starts from the subscription's current plan and changes only
// the flags given; the interval always stays the subscription's.
func planChange(ctx context.Context, d billingDeps, args []string, stdout, stderr io.Writer) int {
	fs := verbFlags("plan change", stderr)
	email := fs.String("email", "", "account email")
	pkg := fs.String("package", "", "package key from the catalog")
	games := fs.String("games", "", "comma-separated games (default: keep the current ones)")
	stores := fs.String("stores", "", "comma-separated store family keys, starter only")
	sub := fs.String("sub", "", "subscription id")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	if !need(stderr, "email", *email) || !need(stderr, "package", *pkg) {
		return 2
	}
	a, ok := byEmail(ctx, d.store, *email, stderr)
	if !ok {
		return 1
	}
	subID := *sub
	if subID == "" {
		ents, err := d.store.ListEntitlements(ctx, a.ID)
		if err != nil {
			return fail(stderr, err)
		}
		if subID, err = billing.SubscriptionFor(ents); err != nil {
			return fail(stderr, err)
		}
	}
	stripeSub, err := d.api.GetSubscription(ctx, subID)
	if err != nil {
		return fail(stderr, fmt.Errorf("billing: fetch %s: %w", subID, err))
	}
	want, _, err := billing.PlanFromMetadata(stripeSub.Metadata)
	if err != nil {
		return fail(stderr, err)
	}
	want.Package = *pkg
	if given["games"] {
		want.Games = splitList(*games)
	}
	if given["stores"] {
		want.Stores = splitList(*stores)
	} else if catPkg, ok := d.cat.Package(*pkg); !ok || catPkg.StoreScope != apiproductlist.StoreScopeExplicit {
		// The old plan's stores only carry over onto a package that still picks stores.
		want.Stores = nil
	}
	rec := newReconciler(d.store, d.api, d.cfg, d.cat, d.stores, stderrAlert(stderr))
	newPlan, err := billing.ChangePlan(ctx, d.api, d.cat, d.stores, d.cfg.GameNames(), a, subID, want, rec.Subscription)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "subscription %s changed to %s\n", subID, newPlan.Describe(d.cat))
	return 0
}
