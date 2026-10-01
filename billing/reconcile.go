package billing

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"strings"
	"sync"
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

	locks subLocks
}

// subLocks is a mutex per subscription id; an entry lives only while held or awaited.
type subLocks struct {
	mu    sync.Mutex
	locks map[string]*subLock
}

type subLock struct {
	mu   sync.Mutex
	refs int
}

// lock blocks until id is free and returns the matching unlock.
func (l *subLocks) lock(id string) func() {
	l.mu.Lock()
	if l.locks == nil {
		l.locks = map[string]*subLock{}
	}
	e := l.locks[id]
	if e == nil {
		e = &subLock{}
		l.locks[id] = e
	}
	e.refs++
	l.mu.Unlock()
	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		l.mu.Lock()
		defer l.mu.Unlock()
		e.refs--
		if e.refs == 0 {
			delete(l.locks, id)
		}
	}
}

// ErrNoAccount means neither the metadata nor the customer id names an account.
var ErrNoAccount = errors.New("billing: subscription names no known account")

// isPermanent reports whether a reconcile error fails the same way on every retry.
func isPermanent(err error) bool {
	if errors.Is(err, ErrStoresUnavailable) {
		return false
	}
	return errors.Is(err, ErrNoPlan) || errors.Is(err, ErrNoAccount) || IsValidation(err)
}

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

// failed names the subscription in err and alerts it, unless a pass reports it in its summary.
func (r *Reconciler) failed(sub *stripe.Subscription, err error, pass bool) error {
	err = fmt.Errorf("subscription %s: %w", sub.ID, err)
	if !pass {
		r.alertf("%v", err)
	}
	return err
}

// MapStatus maps a Stripe status to entitlement status and end: past_due
// keeps grace past periodStart, the unpaid period's start; ended rows end
// at endedAt (or now if zero).
func MapStatus(status stripe.SubscriptionStatus, periodStart, endedAt time.Time, grace time.Duration, now time.Time) (string, *time.Time) {
	switch status {
	case stripe.SubscriptionStatusActive, stripe.SubscriptionStatusTrialing:
		return "active", nil
	case stripe.SubscriptionStatusPastDue:
		until := periodStart.Add(grace)
		return "active", &until
	}
	if endedAt.IsZero() {
		endedAt = now
	}
	return "ended", &endedAt
}

// subPeriodStart is the latest item's current period start, and whether any
// item actually carried one, so a caller can tell a real start from nothing.
func subPeriodStart(sub *stripe.Subscription, now time.Time) (time.Time, bool) {
	var start int64
	if sub.Items != nil {
		for _, it := range sub.Items.Data {
			start = max(start, it.CurrentPeriodStart)
		}
	}
	if start == 0 {
		return now, false
	}
	return time.Unix(start, 0).UTC(), true
}

// subEndedAt is when Stripe ended the subscription, or zero.
func subEndedAt(sub *stripe.Subscription) time.Time {
	if sub.EndedAt == 0 {
		return time.Time{}
	}
	return time.Unix(sub.EndedAt, 0).UTC()
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

// planItems maps each lookup key the plan bills to its quantity.
func planItems(cat *apiproductlist.ProductList, plan Plan) map[string]int64 {
	want := map[string]int64{}
	for _, li := range plan.LineItems(cat) {
		want[li.LookupKey] = li.Quantity
	}
	return want
}

// itemMismatch describes how the subscription's items differ from the plan, or is empty.
func itemMismatch(cat *apiproductlist.ProductList, plan Plan, sub *stripe.Subscription) string {
	want := planItems(cat, plan)
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
	return r.fetchAndApply(ctx, subID, false)
}

// fetchAndApply fetches one subscription and applies it; pass is as for apply.
// The lock covers the fetch too, so an older read cannot land after a newer one.
func (r *Reconciler) fetchAndApply(ctx context.Context, subID string, pass bool) error {
	defer r.locks.lock(subID)()
	sub, err := r.API.GetSubscription(ctx, subID)
	if err != nil {
		return fmt.Errorf("billing: fetch %s: %w", subID, err)
	}
	return r.apply(ctx, sub, pass)
}

// apply upserts the row for a subscription fetched under its lock. Within a
// pass, the pass sends the reload notify and reports the failures.
func (r *Reconciler) apply(ctx context.Context, sub *stripe.Subscription, pass bool) error {
	plan, accountID, err := PlanFromMetadata(sub.Metadata)
	if err != nil {
		return r.failed(sub, err, pass)
	}
	account, err := r.findAccount(ctx, accountID, sub)
	if err != nil {
		return r.failed(sub, err, pass)
	}
	plan, err = plan.Normalize(r.Catalog)
	if err != nil {
		return r.failed(sub, err, pass)
	}
	if msg := itemMismatch(r.Catalog, plan, sub); msg != "" {
		r.alertf("subscription %s: %s; metadata wins, fix the items or the metadata", sub.ID, msg)
	}
	now := r.now()
	start, hasStart := subPeriodStart(sub, now)
	grace := r.Grace
	if !hasStart {
		// No trustworthy period start to anchor grace on: treat the window as already spent.
		grace = 0
	}
	status, until := MapStatus(sub.Status, start, subEndedAt(sub), grace, now)
	resolved, err := plan.Resolve(ctx, r.Catalog, r.Stores)
	if err != nil {
		// Without keys there is nothing to stand in for the scope, so the row cannot be written.
		if status != "ended" || len(plan.Stores) == 0 {
			return r.failed(sub, err, pass)
		}
		r.alertf("subscription %s: %v", sub.ID, err)
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
	if !pass {
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
	// Duplicates counts accounts left holding more than one active stripe row.
	Duplicates int
}

// maxSummaryErrors caps how many errors Summary joins, so one bad pass does
// not produce an unbounded Discord message.
const maxSummaryErrors = 5

// Summary is the one-line Discord message for a pass.
func (res Result) Summary() string {
	dup := ""
	if res.Duplicates == 1 {
		dup = "; 1 account with more than one subscription"
	} else if res.Duplicates > 1 {
		dup = fmt.Sprintf("; %d accounts with more than one subscription", res.Duplicates)
	}
	if res.Failed == 0 {
		return fmt.Sprintf("stripe reconcile: %d subscriptions in sync%s", res.Checked, dup)
	}
	errs := res.Errors
	suffix := ""
	if len(errs) > maxSummaryErrors {
		suffix = fmt.Sprintf(" (+%d more)", len(errs)-maxSummaryErrors)
		errs = errs[:maxSummaryErrors]
	}
	return fmt.Sprintf("stripe reconcile: %d subscriptions, %d failed: %s%s%s", res.Checked, res.Failed, strings.Join(errs, "; "), suffix, dup)
}

// All refetches each listed subscription, as a listing can go stale mid-pass,
// then each active stripe row Stripe did not list, so a missed cancel ends it.
func (r *Reconciler) All(ctx context.Context) (Result, error) {
	var res Result
	subs, err := r.API.ListSubscriptions(ctx)
	if err != nil {
		return res, fmt.Errorf("billing: list subscriptions: %w", err)
	}
	seen := map[string]bool{}
	record := func(subID string, err error) {
		res.Checked++
		if err != nil {
			// The summary caps its list, so the log keeps every failure.
			log.Printf("billing: reconcile %s: %v", subID, err)
			res.Failed++
			res.Errors = append(res.Errors, err.Error())
		}
	}
	for _, sub := range subs {
		seen[sub.ID] = true
		record(sub.ID, r.fetchAndApply(ctx, sub.ID, true))
	}
	refs, err := r.Store.ListActiveStripeRefs(ctx)
	if err != nil {
		return res, fmt.Errorf("billing: list active stripe entitlements: %w", err)
	}
	for _, ref := range refs {
		if !seen[ref.SubID] {
			record(ref.SubID, r.fetchAndApply(ctx, ref.SubID, true))
		}
	}
	// One reload notification for the whole pass, not one per subscription.
	if res.Checked > 0 {
		if err := r.Store.Notify(ctx, ""); err != nil {
			log.Printf("billing: reload notify after reconcile: %v", err)
		}
	}
	// Re-listed, so the check sees the rows the pass just wrote.
	refs, err = r.Store.ListActiveStripeRefs(ctx)
	if err != nil {
		r.alertf("duplicate subscription check: %v", err)
		return res, nil
	}
	res.Duplicates = r.alertDuplicates(refs)
	return res, nil
}

// alertDuplicates alerts once per account holding more than one active stripe row,
// in account order, and returns how many there are.
func (r *Reconciler) alertDuplicates(refs []apiaccess.StripeRef) int {
	subs := map[int64][]string{}
	for _, ref := range refs {
		subs[ref.AccountID] = append(subs[ref.AccountID], ref.SubID)
	}
	n := 0
	for _, id := range slices.Sorted(maps.Keys(subs)) {
		if ids := subs[id]; len(ids) > 1 {
			slices.Sort(ids)
			r.alertf("account %d has %d active Stripe subscriptions (%s)", id, len(ids), strings.Join(ids, ", "))
			n++
		}
	}
	return n
}
