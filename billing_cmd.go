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

var billingUsage = map[string]string{
	"catalog":  "seed",
	"checkout": "link",
	"invite":   "create",
	"stripe":   "reconcile",
	"portal":   "link",
	"plan":     "change",
}

func init() {
	for name := range billingUsage {
		name := name
		commands[name] = command{
			usage: billingUsage[name],
			run: func(ctx context.Context, args []string, stdout, stderr io.Writer) int {
				return withStore(ctx, args, stderr, func(store *apiaccess.Client, cfg *config.Config, rest []string) int {
					api, err := stripeFromEnv()
					if err != nil {
						fmt.Fprintln(stderr, "api-gatewahy:", err)
						return 1
					}
					return runBilling(ctx, billingDeps{store: store, api: api, cfg: cfg, cat: apiproductlist.MustLoad()}, name, rest, stdout, stderr)
				})
			},
		}
	}
}

// billingDeps is what the billing verbs need; tests leave store and api nil
// for the paths that fail before reaching them.
type billingDeps struct {
	store *apiaccess.Client
	api   billing.API
	cfg   *config.Config
	cat   *apiproductlist.ProductList
}

// stripeFromEnv builds the Stripe client; the key never lives in the config file.
func stripeFromEnv() (billing.API, error) {
	key := os.Getenv("STRIPE_SECRET_KEY")
	if key == "" {
		return nil, errors.New("STRIPE_SECRET_KEY is not set")
	}
	return billing.NewClient(key), nil
}

func newCheckout(store billing.Store, api billing.API, cfg *config.Config, cat *apiproductlist.ProductList) *billing.Checkout {
	success, cancel := cfg.CheckoutURLs()
	return &billing.Checkout{Store: store, API: api, Catalog: cat, Games: cfg.GameNames(), SuccessURL: success, CancelURL: cancel}
}

func newReconciler(store billing.Store, api billing.API, cfg *config.Config, cat *apiproductlist.ProductList, alert func(string)) *billing.Reconciler {
	return &billing.Reconciler{Store: store, API: api, Catalog: cat, Grace: time.Duration(cfg.Stripe.GraceDays) * 24 * time.Hour, Alert: alert}
}

// runBilling dispatches one billing verb. Exit codes: 0 ok, 1 error, 2 usage.
func runBilling(ctx context.Context, d billingDeps, cmd string, args []string, stdout, stderr io.Writer) int {
	fail := func(err error) int {
		fmt.Fprintln(stderr, "api-gatewahy:", err)
		return 1
	}
	usage := func() int {
		fmt.Fprintf(stderr, "usage: api-gatewahy %s <%s>\n", cmd, billingUsage[cmd])
		return 2
	}
	if len(args) == 0 {
		return usage()
	}
	verb, args := args[0], args[1:]
	fs := flag.NewFlagSet(cmd+" "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	email := fs.String("email", "", "account email")
	pkg := fs.String("package", "", "package key from the catalog")
	games := fs.String("games", "magic", "comma-separated games")
	stores := fs.String("stores", "", "comma-separated stores, starter only")
	interval := fs.String("interval", "monthly", "interval key")
	invite := fs.String("invite", "", "invite token for a non-public interval")
	days := fs.Int("days", 14, "invite lifetime in days")
	note := fs.String("note", "", "free-text note")
	sub := fs.String("sub", "", "subscription id")
	ret := fs.String("return", "", "portal return URL (default public_url)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	need := func(name, val string) bool {
		if val == "" {
			fmt.Fprintf(stderr, "api-gatewahy: -%s is required\n", name)
			return false
		}
		return true
	}
	byEmail := func() (apiaccess.Account, bool) {
		if !need("email", *email) {
			return apiaccess.Account{}, false
		}
		a, err := d.store.GetAccountByEmail(ctx, *email)
		if err != nil {
			fail(fmt.Errorf("%s: %w", *email, err))
			return apiaccess.Account{}, false
		}
		return a, true
	}
	plan := func() billing.Plan {
		return billing.Plan{Package: *pkg, Interval: *interval, Games: splitList(*games), Stores: splitList(*stores)}
	}
	alert := func(msg string) { fmt.Fprintln(stderr, "alert:", msg) }

	switch cmd + " " + verb {
	case "catalog seed":
		res, err := billing.Seed(ctx, d.api, d.cat)
		if err != nil {
			return fail(err)
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

	case "checkout link":
		if !need("email", *email) || !need("package", *pkg) {
			return 2
		}
		a, ok := byEmail()
		if !ok {
			return 1
		}
		url, err := newCheckout(d.store, d.api, d.cfg, d.cat).Create(ctx, billing.Request{Account: a, Plan: plan(), Invite: *invite})
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "checkout link for %s, valid 24 hours:\n\n    %s\n\n", a.Email, url)
		return 0

	case "invite create":
		if !need("interval", *interval) {
			return 2
		}
		iv, ok := d.cat.Interval(*interval)
		if !ok {
			return fail(fmt.Errorf("unknown interval %q", *interval))
		}
		if iv.Public {
			return fail(fmt.Errorf("interval %s is public and does not need an invite", iv.Key))
		}
		token, inv, err := d.store.CreateInvite(ctx, iv.Key, *email, time.Duration(*days)*24*time.Hour, *note)
		if err != nil {
			return fail(err)
		}
		bound := "any account"
		if inv.Email != "" {
			bound = inv.Email
		}
		fmt.Fprintf(stdout, "invite for %s (%s), expires %s. Shown once, copy it now:\n\n    %s\n\n", iv.Key, bound, inv.ExpiresAt.Format("2006-01-02"), token)
		return 0

	case "stripe reconcile":
		rec := newReconciler(d.store, d.api, d.cfg, d.cat, alert)
		if *sub != "" {
			if err := rec.Subscription(ctx, *sub); err != nil {
				return fail(err)
			}
			fmt.Fprintf(stdout, "subscription %s reconciled\n", *sub)
			return 0
		}
		res, err := rec.All(ctx)
		if err != nil {
			return fail(err)
		}
		fmt.Fprintln(stdout, res.Summary())
		if res.Failed > 0 {
			return 1
		}
		return 0

	case "portal link":
		a, ok := byEmail()
		if !ok {
			return exitFor(*email)
		}
		returnURL := *ret
		if returnURL == "" {
			returnURL = d.cfg.PublicURL
		}
		url, err := billing.PortalURL(ctx, d.api, a, returnURL)
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "portal link for %s:\n\n    %s\n\n", a.Email, url)
		return 0

	case "plan change":
		if !need("email", *email) || !need("package", *pkg) {
			return 2
		}
		a, ok := byEmail()
		if !ok {
			return 1
		}
		subID := *sub
		if subID == "" {
			ents, err := d.store.ListEntitlements(ctx, a.ID)
			if err != nil {
				return fail(err)
			}
			if subID, err = billing.SubscriptionFor(ents); err != nil {
				return fail(err)
			}
		}
		rec := newReconciler(d.store, d.api, d.cfg, d.cat, alert)
		newPlan, err := billing.ChangePlan(ctx, d.api, d.cat, d.cfg.GameNames(), a, subID, plan(), rec.Subscription)
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "subscription %s changed to %s\n", subID, newPlan.Describe(d.cat))
		return 0
	}
	return usage()
}
