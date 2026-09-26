package billing

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"strings"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/mtgban-website/apiproductlist"
	"github.com/stripe/stripe-go/v84"
)

// Reconciler turns Stripe subscriptions into entitlement rows.
type Reconciler struct {
	Store   Store
	API     API
	Catalog *apiproductlist.ProductList
	Stores  StoreLister
	Grace   time.Duration
	Alert   func(string)
	Now     func() time.Time
}

// ErrNoAccount means neither the metadata nor the customer id names an account.
var ErrNoAccount = errors.New("billing: subscription names no known account")

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reconciler) alertf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Println("billing:", msg)
	if r.Alert != nil {
		r.Alert("api-gatewahy: " + msg)
	}
}

// MapStatus is the spec's table from Stripe status to entitlement status
// and end. past_due keeps access for grace past the period end.
func MapStatus(status stripe.SubscriptionStatus, periodEnd time.Time, grace time.Duration, now time.Time) (string, *time.Time) {
	switch status {
	case stripe.SubscriptionStatusActive, stripe.SubscriptionStatusTrialing:
		return "active", nil
	case stripe.SubscriptionStatusPastDue:
		until := periodEnd.Add(grace)
		return "active", &until
	}
	return "ended", &now
}

// subPeriodEnd is the latest item period end, or now when the items carry none.
func subPeriodEnd(sub *stripe.Subscription, now time.Time) time.Time {
	var end int64
	if sub.Items != nil {
		for _, it := range sub.Items.Data {
			end = max(end, it.CurrentPeriodEnd)
		}
	}
	if end == 0 {
		return now
	}
	return time.Unix(end, 0).UTC()
}

// priceLookupKey is p.LookupKey, or the key rebuilt from the metadata Seed
// writes when a replacement price left LookupKey cleared.
func priceLookupKey(p *stripe.Price) string {
	if p.LookupKey != "" {
		return p.LookupKey
	}
	item := p.Metadata["package"]
	if item == "" {
		item = p.Metadata["addon"]
	}
	iv := p.Metadata["interval_key"]
	if item == "" || iv == "" {
		return ""
	}
	return apiproductlist.LookupKey(item, iv)
}

// itemMismatch describes how the subscription's items differ from the plan, or is empty.
func itemMismatch(cat *apiproductlist.ProductList, plan Plan, sub *stripe.Subscription) string {
	want := map[string]int64{}
	for _, li := range plan.LineItems(cat) {
		want[li.LookupKey] = li.Quantity
	}
	got := map[string]int64{}
	if sub.Items != nil {
		for _, it := range sub.Items.Data {
			if it.Price != nil {
				got[priceLookupKey(it.Price)] += it.Quantity
			}
		}
	}
	if maps.Equal(want, got) {
		return ""
	}
	return fmt.Sprintf("items %v do not match the plan's %v", got, want)
}

// Subscription fetches one subscription from Stripe and applies it.
func (r *Reconciler) Subscription(ctx context.Context, subID string) error {
	return r.fetchAndApply(ctx, subID, true)
}

// fetchAndApply fetches one subscription and applies it, notifying the
// resolver cache only when notify is set.
func (r *Reconciler) fetchAndApply(ctx context.Context, subID string, notify bool) error {
	sub, err := r.API.GetSubscription(ctx, subID)
	if err != nil {
		return fmt.Errorf("billing: fetch %s: %w", subID, err)
	}
	return r.apply(ctx, sub, notify)
}

// apply upserts the entitlement row for a fetched subscription.
func (r *Reconciler) apply(ctx context.Context, sub *stripe.Subscription, notify bool) error {
	plan, accountID, err := PlanFromMetadata(sub.Metadata)
	if err != nil {
		r.alertf("subscription %s: %v", sub.ID, err)
		return err
	}
	account, err := r.findAccount(ctx, accountID, sub)
	if err != nil {
		r.alertf("subscription %s: %v", sub.ID, err)
		return err
	}
	plan, err = plan.Normalize(r.Catalog)
	if err != nil {
		r.alertf("subscription %s: %v", sub.ID, err)
		return err
	}
	if msg := itemMismatch(r.Catalog, plan, sub); msg != "" {
		r.alertf("subscription %s: %s; metadata wins, fix the items or the metadata", sub.ID, msg)
	}
	now := r.now()
	status, until := MapStatus(sub.Status, subPeriodEnd(sub, now), r.Grace, now)
	resolved, err := plan.Resolve(ctx, r.Catalog, r.Stores)
	if err != nil {
		r.alertf("subscription %s: %v", sub.ID, err)
		// Without keys there is nothing to stand in for the scope, so the row cannot be written.
		if status != "ended" || len(plan.Stores) == 0 {
			return err
		}
		// An ended row grants nothing, so the keys stand in for the scope and access still ends.
		pkg, _ := r.Catalog.Package(plan.Package)
		resolved = ResolvedPlan{Plan: plan, Scope: strings.Join(plan.Stores, ","), Modes: pkg.Modes}
	}
	e := apiaccess.Entitlement{
		AccountID:   account.ID,
		Source:      "stripe",
		Games:       plan.Games,
		StoreScope:  resolved.Scope,
		Modes:       resolved.Modes,
		Addons:      plan.Addons(r.Catalog),
		Status:      status,
		ValidUntil:  until,
		ExternalRef: sub.ID,
		Note:        plan.Describe(r.Catalog),
	}
	if _, err := r.Store.UpsertStripeEntitlement(ctx, e); err != nil {
		return fmt.Errorf("billing: upsert %s: %w", sub.ID, err)
	}
	if notify {
		if err := r.Store.Notify(ctx, ""); err != nil {
			log.Printf("billing: reload notify after %s: %v", sub.ID, err)
		}
	}
	return nil
}

// findAccount tries metadata.account_id, then the customer id.
func (r *Reconciler) findAccount(ctx context.Context, accountID int64, sub *stripe.Subscription) (apiaccess.Account, error) {
	if accountID != 0 {
		a, err := r.Store.GetAccount(ctx, accountID)
		if err == nil {
			return a, nil
		}
		if !errors.Is(err, apiaccess.ErrNotFound) {
			return apiaccess.Account{}, err
		}
	}
	if sub.Customer != nil && sub.Customer.ID != "" {
		a, err := r.Store.GetAccountByStripeCustomer(ctx, sub.Customer.ID)
		if err == nil {
			return a, nil
		}
		if !errors.Is(err, apiaccess.ErrNotFound) {
			return apiaccess.Account{}, err
		}
	}
	return apiaccess.Account{}, ErrNoAccount
}

// Result summarizes a full pass.
type Result struct {
	Checked int
	Failed  int
	Errors  []string
}

// maxSummaryErrors caps how many errors Summary joins, so one bad pass does
// not produce an unbounded Discord message.
const maxSummaryErrors = 5

// Summary is the one-line Discord message for a pass.
func (res Result) Summary() string {
	if res.Failed == 0 {
		return fmt.Sprintf("stripe reconcile: %d subscriptions in sync", res.Checked)
	}
	errs := res.Errors
	suffix := ""
	if len(errs) > maxSummaryErrors {
		suffix = fmt.Sprintf(" (+%d more)", len(errs)-maxSummaryErrors)
		errs = errs[:maxSummaryErrors]
	}
	return fmt.Sprintf("stripe reconcile: %d subscriptions, %d failed: %s%s", res.Checked, res.Failed, strings.Join(errs, "; "), suffix)
}

// All applies every subscription Stripe lists, then fetches every active
// stripe entitlement Stripe did not list so a missed cancellation still ends it.
func (r *Reconciler) All(ctx context.Context) (Result, error) {
	var res Result
	subs, err := r.API.ListSubscriptions(ctx)
	if err != nil {
		return res, fmt.Errorf("billing: list subscriptions: %w", err)
	}
	seen := map[string]bool{}
	record := func(err error) {
		res.Checked++
		if err != nil {
			res.Failed++
			res.Errors = append(res.Errors, err.Error())
		}
	}
	for _, sub := range subs {
		seen[sub.ID] = true
		record(r.apply(ctx, sub, false))
	}
	refs, err := r.Store.ListActiveStripeRefs(ctx)
	if err != nil {
		return res, fmt.Errorf("billing: list active stripe entitlements: %w", err)
	}
	for _, ref := range refs {
		if !seen[ref] {
			record(r.fetchAndApply(ctx, ref, false))
		}
	}
	// One reload notification for the whole pass, not one per subscription.
	if res.Checked > 0 {
		if err := r.Store.Notify(ctx, ""); err != nil {
			log.Printf("billing: reload notify after reconcile: %v", err)
		}
	}
	return res, nil
}
