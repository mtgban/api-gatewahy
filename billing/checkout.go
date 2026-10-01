package billing

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/mtgban-website/apiproductlist"
	"github.com/stripe/stripe-go/v84"
)

// Checkout builds hosted Checkout Sessions for plans.
type Checkout struct {
	Store      Store
	API        API
	Catalog    *apiproductlist.ProductList
	Stores     StoreLister
	Games      []string
	SuccessURL string
	CancelURL  string
	Now        func() time.Time
}

// Request is one checkout: the account, the plan, and an invite when the
// interval is not public.
type Request struct {
	Account apiaccess.Account
	Plan    Plan
	Invite  string
	// Resolved is the caller's Resolve of Plan, reused so a request resolves once.
	Resolved *ResolvedPlan
}

// releaseTimeout bounds the invite release that outlives the request; a var for tests.
var releaseTimeout = 10 * time.Second

// ErrPriceNotSeeded means Stripe has no active Price for a lookup key.
var ErrPriceNotSeeded = errors.New("billing: price not seeded; run catalog seed")

// ErrHasPlan means the account already pays through Stripe; a change goes through ChangePlan.
var ErrHasPlan = errors.New("billing: account already has an active Stripe plan")

func (c *Checkout) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Session is a created Checkout Session: the URL to send the customer to and
// the id to expire if they come back without paying.
type Session struct {
	ID  string
	URL string
}

// Create refuses an account with a Stripe plan, validates the plan, consumes a needed invite,
// ensures the Stripe customer, expires its other open sessions, and returns a new one.
func (c *Checkout) Create(ctx context.Context, req Request) (sess Session, err error) {
	if req.Account.Status != "active" {
		return Session{}, fmt.Errorf("billing: account %d is %s", req.Account.ID, req.Account.Status)
	}
	ents, err := c.Store.ListEntitlements(ctx, req.Account.ID)
	if err != nil {
		return Session{}, fmt.Errorf("billing: list entitlements: %w", err)
	}
	if apiaccess.HasActiveStripePlan(ents, c.now()) {
		return Session{}, ErrHasPlan
	}
	plan, err := req.Plan.Validate(c.Catalog, c.Games, req.Invite != "")
	if err != nil {
		return Session{}, err
	}
	if req.Resolved == nil || !req.Resolved.equal(plan) {
		if _, err := plan.Resolve(ctx, c.Catalog, c.Stores); err != nil {
			return Session{}, err
		}
	}
	if iv, _ := c.Catalog.Interval(plan.Interval); !iv.Public {
		// Assigned, not declared, so the deferred release sees this err.
		var inv apiaccess.Invite
		inv, err = c.Store.ConsumeInvite(ctx, req.Invite, req.Account.Email, c.now())
		if err != nil {
			return Session{}, err
		}
		defer func() {
			if err == nil {
				return
			}
			// Released even when the client has gone, or the invite stays spent.
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
			defer cancel()
			if rerr := c.Store.ReleaseInvite(rctx, req.Invite); rerr != nil {
				log.Printf("billing: release invite for account %d: %v", req.Account.ID, rerr)
			}
		}()
		if inv.IntervalKey != plan.Interval {
			return Session{}, fmt.Errorf("billing: invite is for %s, not %s", inv.IntervalKey, plan.Interval)
		}
	}
	lineItems, err := c.lineItems(ctx, plan)
	if err != nil {
		return Session{}, err
	}
	customerID, created, err := c.ensureCustomer(ctx, req.Account)
	if err != nil {
		return Session{}, err
	}
	// Another tab's session could still be paid, giving the account a second subscription.
	if !created {
		if err = c.expireOpenSessions(ctx, req.Account.ID, customerID); err != nil {
			return Session{}, err
		}
	}
	cs, err := c.API.CreateCheckoutSession(ctx, &stripe.CheckoutSessionCreateParams{
		Mode:                stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		Customer:            stripe.String(customerID),
		ClientReferenceID:   stripe.String(strconv.FormatInt(req.Account.ID, 10)),
		AllowPromotionCodes: stripe.Bool(true),
		SuccessURL:          stripe.String(c.SuccessURL),
		CancelURL:           stripe.String(c.CancelURL),
		LineItems:           lineItems,
		SubscriptionData:    &stripe.CheckoutSessionCreateSubscriptionDataParams{Metadata: plan.Metadata(req.Account.ID)},
	})
	if err != nil {
		return Session{}, fmt.Errorf("billing: create checkout session: %w", err)
	}
	return Session{ID: cs.ID, URL: cs.URL}, nil
}

// Abandon ends a Checkout Session the customer walked away from and, once
// Stripe confirms it expired, hands the invite back. A session that already
// completed cannot be expired, so its invite stays spent.
func (c *Checkout) Abandon(ctx context.Context, sessionID, invite string) error {
	cs, err := c.API.ExpireCheckoutSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("billing: expire checkout session: %w", err)
	}
	if cs.Status != stripe.CheckoutSessionStatusExpired {
		return fmt.Errorf("billing: checkout session %s is %s, not expired", sessionID, cs.Status)
	}
	if invite == "" {
		return nil
	}
	return c.Store.ReleaseInvite(ctx, invite)
}

// expireOpenSessions expires the customer's open Checkout Sessions. Stripe may
// have completed one meanwhile, so a failed expire is logged, not returned.
func (c *Checkout) expireOpenSessions(ctx context.Context, accountID int64, customerID string) error {
	open, err := c.API.ListOpenCheckoutSessions(ctx, customerID)
	if err != nil {
		return fmt.Errorf("billing: list open checkout sessions: %w", err)
	}
	for _, cs := range open {
		if _, err := c.API.ExpireCheckoutSession(ctx, cs.ID); err != nil {
			log.Printf("billing: expire checkout session %s for account %d: %v", cs.ID, accountID, err)
		}
	}
	return nil
}

// ensureCustomer returns the account's Stripe customer, creating one if
// needed; created reports that this call made it, so it has no sessions yet.
func (c *Checkout) ensureCustomer(ctx context.Context, a apiaccess.Account) (id string, created bool, err error) {
	if a.StripeCustomerID != "" {
		return a.StripeCustomerID, false, nil
	}
	cust, err := c.API.CreateCustomer(ctx, &stripe.CustomerCreateParams{
		Email:    stripe.String(a.Email),
		Metadata: map[string]string{"account_id": strconv.FormatInt(a.ID, 10)},
	})
	if err != nil {
		return "", false, fmt.Errorf("billing: create customer: %w", err)
	}
	// Two racing checkouts may both create one; the first stored id wins.
	id, err = c.Store.SetStripeCustomerID(ctx, a.ID, cust.ID)
	if err != nil {
		return "", false, fmt.Errorf("billing: store customer: %w", err)
	}
	return id, id == cust.ID, nil
}

func (c *Checkout) lineItems(ctx context.Context, plan Plan) ([]*stripe.CheckoutSessionCreateLineItemParams, error) {
	ids, err := priceIDs(ctx, c.API)
	if err != nil {
		return nil, fmt.Errorf("billing: list prices: %w", err)
	}
	var out []*stripe.CheckoutSessionCreateLineItemParams
	for _, li := range plan.LineItems(c.Catalog) {
		id, ok := ids[li.LookupKey]
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrPriceNotSeeded, li.LookupKey)
		}
		out = append(out, &stripe.CheckoutSessionCreateLineItemParams{Price: stripe.String(id), Quantity: stripe.Int64(li.Quantity)})
	}
	return out, nil
}
