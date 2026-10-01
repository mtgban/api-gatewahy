package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/api-gatewahy/billing"
	"github.com/mtgban/api-gatewahy/config"
	"github.com/mtgban/mtgban-website/apiproductlist"
	"github.com/stripe/stripe-go/v84"
)

func TestBillingUsageErrors(t *testing.T) {
	d := billingDeps{cat: apiproductlist.MustLoad()}
	cases := []struct {
		name string
		cmd  string
		args []string
		want string
	}{
		{"no verb", "checkout", nil, "usage: api-gatewahy checkout"},
		{"unknown verb", "checkout", []string{"pay"}, "usage: api-gatewahy checkout"},
		{"link without email", "checkout", []string{"link", "-package", "all_data"}, "-email is required"},
		{"link without package", "checkout", []string{"link", "-email", "x@example.com"}, "-package is required"},
		{"invite unknown interval", "invite", []string{"create", "-interval", "weekly"}, "unknown interval"},
		{"invite public interval", "invite", []string{"create", "-interval", "monthly"}, "does not need an invite"},
		{"portal without email", "portal", []string{"link"}, "-email is required"},
		{"plan without package", "plan", []string{"change", "-email", "x@example.com"}, "-package is required"},
		{"bad flag", "stripe", []string{"reconcile", "-bogus"}, "flag provided but not defined"},
	}
	for _, c := range cases {
		var out, errb bytes.Buffer
		if code := runBilling(context.Background(), d, c.cmd, c.args, &out, &errb); code != 2 && code != 1 {
			t.Errorf("%s: exit %d", c.name, code)
		}
		if !strings.Contains(errb.String(), c.want) {
			t.Errorf("%s: stderr %q lacks %q", c.name, errb.String(), c.want)
		}
	}
}

func TestStripeSubscriptionFor(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	stripeActive := apiaccess.Entitlement{Source: "stripe", Status: "active", ExternalRef: "sub_1"}
	stripeEnded := apiaccess.Entitlement{Source: "stripe", Status: "ended", ExternalRef: "sub_0"}
	manual := apiaccess.Entitlement{Source: "manual", Status: "active"}
	if id, err := billing.SubscriptionFor([]apiaccess.Entitlement{manual, stripeEnded, stripeActive}); err != nil || id != "sub_1" {
		t.Errorf("one live: %q %v", id, err)
	}
	if _, err := billing.SubscriptionFor([]apiaccess.Entitlement{manual, stripeEnded}); err == nil {
		t.Error("none live accepted")
	}
	other := stripeActive
	other.ExternalRef = "sub_2"
	if _, err := billing.SubscriptionFor([]apiaccess.Entitlement{stripeActive, other}); err == nil || !strings.Contains(err.Error(), "-sub") {
		t.Errorf("two live: %v", err)
	}
	// Grace lapsing does not end the Stripe subscription, so a lapsed row is
	// still a candidate: two of them is still "more than one" to resolve.
	lapsed := other
	lapsed.ValidUntil = &past
	if _, err := billing.SubscriptionFor([]apiaccess.Entitlement{stripeActive, lapsed}); !errors.Is(err, billing.ErrManySubscriptions) {
		t.Errorf("lapsed-but-active row dropped: %v", err)
	}
	// An ended row is never a candidate, lapsed or not.
	endedLapsed := stripeEnded
	endedLapsed.ValidUntil = &past
	if id, err := billing.SubscriptionFor([]apiaccess.Entitlement{endedLapsed, stripeActive}); err != nil || id != "sub_1" {
		t.Errorf("ended row picked: %q %v", id, err)
	}
}

func TestBillingCommandsRegistered(t *testing.T) {
	for _, name := range []string{"catalog", "checkout", "invite", "stripe", "portal", "plan"} {
		if _, ok := commands[name]; !ok {
			t.Errorf("command %s not registered", name)
		}
	}
}

// fakeBillingAPI is an in-memory billing.API with just enough behavior for
// the CLI tests: it fetches and updates subscriptions and creates checkout
// sessions, applying item adds/removes/quantity changes like Stripe would.
type fakeBillingAPI struct {
	seq      int
	subs     map[string]*stripe.Subscription
	prices   map[string]*stripe.Price
	updates  []*stripe.SubscriptionUpdateParams
	sessions []*stripe.CheckoutSession
}

var _ billing.API = (*fakeBillingAPI)(nil)

func newFakeBillingAPI() *fakeBillingAPI {
	return &fakeBillingAPI{subs: map[string]*stripe.Subscription{}, prices: map[string]*stripe.Price{}}
}

func (f *fakeBillingAPI) next(prefix string) string {
	f.seq++
	return fmt.Sprintf("%s_%d", prefix, f.seq)
}

// addPrice seeds an active price for a lookup key, as `catalog seed` would.
func (f *fakeBillingAPI) addPrice(lookupKey string) {
	id := f.next("price")
	f.prices[id] = &stripe.Price{ID: id, Active: true, LookupKey: lookupKey}
}

func (f *fakeBillingAPI) priceByKey(key string) *stripe.Price {
	for _, p := range f.prices {
		if p.LookupKey == key {
			return p
		}
	}
	return nil
}

// addSub installs a subscription whose items reference prices by lookup key.
func (f *fakeBillingAPI) addSub(t *testing.T, id string, metadata map[string]string, items map[string]int64) *stripe.Subscription {
	t.Helper()
	s := &stripe.Subscription{ID: id, Status: stripe.SubscriptionStatusActive, Metadata: metadata, Items: &stripe.SubscriptionItemList{}}
	for key, qty := range items {
		p := f.priceByKey(key)
		if p == nil {
			t.Fatalf("no price for %s; call addPrice first", key)
		}
		s.Items.Data = append(s.Items.Data, &stripe.SubscriptionItem{
			ID: f.next("si"), Price: p, Quantity: qty, CurrentPeriodEnd: time.Now().Add(30 * 24 * time.Hour).Unix(),
		})
	}
	f.subs[id] = s
	return s
}

func (f *fakeBillingAPI) GetSubscription(_ context.Context, id string) (*stripe.Subscription, error) {
	s, ok := f.subs[id]
	if !ok {
		return nil, fmt.Errorf("fake stripe: no subscription %s", id)
	}
	return s, nil
}

func (f *fakeBillingAPI) UpdateSubscription(_ context.Context, id string, p *stripe.SubscriptionUpdateParams) (*stripe.Subscription, error) {
	s, ok := f.subs[id]
	if !ok {
		return nil, fmt.Errorf("fake stripe: no subscription %s", id)
	}
	f.updates = append(f.updates, p)
	if p.Metadata != nil {
		s.Metadata = p.Metadata
	}
	for _, item := range p.Items {
		switch {
		case item.Deleted != nil && *item.Deleted:
			kept := s.Items.Data[:0]
			for _, it := range s.Items.Data {
				if it.ID != *item.ID {
					kept = append(kept, it)
				}
			}
			s.Items.Data = kept
		case item.ID != nil:
			for _, it := range s.Items.Data {
				if it.ID == *item.ID && item.Quantity != nil {
					it.Quantity = *item.Quantity
				}
			}
		case item.Price != nil:
			s.Items.Data = append(s.Items.Data, &stripe.SubscriptionItem{
				ID: f.next("si"), Price: f.prices[*item.Price], Quantity: stripe.Int64Value(item.Quantity),
			})
		}
	}
	return s, nil
}

func (f *fakeBillingAPI) ListPrices(context.Context) ([]*stripe.Price, error) {
	var out []*stripe.Price
	for _, p := range f.prices {
		out = append(out, p)
	}
	return out, nil
}

func (f *fakeBillingAPI) CreateCheckoutSession(_ context.Context, p *stripe.CheckoutSessionCreateParams) (*stripe.CheckoutSession, error) {
	id := f.next("cs")
	cs := &stripe.CheckoutSession{ID: id, URL: "https://checkout.stripe.test/" + id}
	f.sessions = append(f.sessions, cs)
	return cs, nil
}

func (f *fakeBillingAPI) notImplemented(method string) error {
	return fmt.Errorf("fake stripe: %s not implemented", method)
}

func (f *fakeBillingAPI) CreateCustomer(context.Context, *stripe.CustomerCreateParams) (*stripe.Customer, error) {
	return nil, f.notImplemented("CreateCustomer")
}
func (f *fakeBillingAPI) ExpireCheckoutSession(context.Context, string) (*stripe.CheckoutSession, error) {
	return nil, f.notImplemented("ExpireCheckoutSession")
}

// ListOpenCheckoutSessions reports none open, so checkout has nothing to expire.
func (f *fakeBillingAPI) ListOpenCheckoutSessions(context.Context, string) ([]*stripe.CheckoutSession, error) {
	return nil, nil
}
func (f *fakeBillingAPI) ListSubscriptions(context.Context) ([]*stripe.Subscription, error) {
	return nil, f.notImplemented("ListSubscriptions")
}
func (f *fakeBillingAPI) GetProduct(context.Context, string) (*stripe.Product, error) {
	return nil, f.notImplemented("GetProduct")
}
func (f *fakeBillingAPI) CreateProduct(context.Context, *stripe.ProductCreateParams) (*stripe.Product, error) {
	return nil, f.notImplemented("CreateProduct")
}
func (f *fakeBillingAPI) UpdateProduct(context.Context, string, *stripe.ProductUpdateParams) (*stripe.Product, error) {
	return nil, f.notImplemented("UpdateProduct")
}
func (f *fakeBillingAPI) CreatePrice(context.Context, *stripe.PriceCreateParams) (*stripe.Price, error) {
	return nil, f.notImplemented("CreatePrice")
}
func (f *fakeBillingAPI) UpdatePrice(context.Context, string, *stripe.PriceUpdateParams) (*stripe.Price, error) {
	return nil, f.notImplemented("UpdatePrice")
}
func (f *fakeBillingAPI) CreatePortalSession(context.Context, *stripe.BillingPortalSessionCreateParams) (*stripe.BillingPortalSession, error) {
	return nil, f.notImplemented("CreatePortalSession")
}

// fakeBillingStore is billingStore over plain maps, keyed by account id and email.
type fakeBillingStore struct {
	accounts     map[int64]apiaccess.Account
	byEmail      map[string]apiaccess.Account
	entitlements map[int64][]apiaccess.Entitlement
	upserts      []apiaccess.Entitlement
}

var _ billingStore = (*fakeBillingStore)(nil)

func newFakeBillingStore() *fakeBillingStore {
	return &fakeBillingStore{accounts: map[int64]apiaccess.Account{}, byEmail: map[string]apiaccess.Account{}, entitlements: map[int64][]apiaccess.Entitlement{}}
}

func (f *fakeBillingStore) addAccount(a apiaccess.Account) {
	f.accounts[a.ID] = a
	f.byEmail[a.Email] = a
}

func (f *fakeBillingStore) GetAccount(_ context.Context, id int64) (apiaccess.Account, error) {
	a, ok := f.accounts[id]
	if !ok {
		return apiaccess.Account{}, apiaccess.ErrNotFound
	}
	return a, nil
}

func (f *fakeBillingStore) GetAccountByEmail(_ context.Context, email string) (apiaccess.Account, error) {
	a, ok := f.byEmail[email]
	if !ok {
		return apiaccess.Account{}, apiaccess.ErrNotFound
	}
	return a, nil
}

func (f *fakeBillingStore) GetAccountByStripeCustomer(context.Context, string) (apiaccess.Account, error) {
	return apiaccess.Account{}, apiaccess.ErrNotFound
}

func (f *fakeBillingStore) SetStripeCustomerID(context.Context, int64, string) (string, error) {
	return "", errors.New("fake store: SetStripeCustomerID not implemented")
}

func (f *fakeBillingStore) ConsumeInvite(context.Context, string, string, time.Time) (apiaccess.Invite, error) {
	return apiaccess.Invite{}, errors.New("fake store: ConsumeInvite not implemented")
}

func (f *fakeBillingStore) ReleaseInvite(context.Context, string) error { return nil }

func (f *fakeBillingStore) UpsertStripeEntitlement(_ context.Context, e apiaccess.Entitlement) (apiaccess.Entitlement, error) {
	f.upserts = append(f.upserts, e)
	return e, nil
}

func (f *fakeBillingStore) ListActiveStripeRefs(context.Context) ([]apiaccess.StripeRef, error) {
	return nil, nil
}

func (f *fakeBillingStore) Notify(context.Context, string) error { return nil }

func (f *fakeBillingStore) ListEntitlements(_ context.Context, accountID int64) ([]apiaccess.Entitlement, error) {
	return f.entitlements[accountID], nil
}

func (f *fakeBillingStore) CreateInvite(context.Context, string, string, time.Duration, string) (string, apiaccess.Invite, error) {
	return "", apiaccess.Invite{}, errors.New("fake store: CreateInvite not implemented")
}

// billingTestDeps builds billingDeps with a magic+pokemon catalog and both fakes wired in.
func billingTestDeps(api *fakeBillingAPI, store *fakeBillingStore) billingDeps {
	cfg := &config.Config{Games: map[string]config.Game{"magic": {}, "pokemon": {}}, Stripe: config.StripeConfig{GraceDays: 10}}
	return billingDeps{store: store, api: api, cfg: cfg, cat: apiproductlist.MustLoad()}
}

// TestPlanChangeKeepsGamesUnlessGiven is issue #33 item 1: plan change must
// start from the subscription's current plan and only override flags given.
func TestPlanChangeKeepsGamesUnlessGiven(t *testing.T) {
	pkgKey, gamesKey := apiproductlist.LookupKey("all_data", "monthly"), apiproductlist.LookupKey("extra_game", "monthly")

	newFixture := func(t *testing.T, subID string) (*fakeBillingAPI, *fakeBillingStore, apiaccess.Account) {
		api := newFakeBillingAPI()
		api.addPrice(pkgKey)
		api.addPrice(gamesKey)
		account := apiaccess.Account{ID: 1, Email: "x@example.com", Status: "active"}
		api.addSub(t, subID, map[string]string{
			"package": "all_data", "interval": "monthly", "games": "magic,pokemon", "stores": "", "account_id": "1",
		}, map[string]int64{pkgKey: 1, gamesKey: 1})
		store := newFakeBillingStore()
		store.addAccount(account)
		store.entitlements[1] = []apiaccess.Entitlement{{Source: "stripe", Status: "active", ExternalRef: subID}}
		return api, store, account
	}

	t.Run("games omitted keeps pokemon", func(t *testing.T) {
		api, store, _ := newFixture(t, "sub_1")
		d := billingTestDeps(api, store)
		var out, errb bytes.Buffer
		code := runBilling(context.Background(), d, "plan", []string{"change", "-email", "x@example.com", "-package", "all_data"}, &out, &errb)
		if code != 0 {
			t.Fatalf("exit %d, stderr %q", code, errb.String())
		}
		if len(api.updates) == 0 {
			t.Fatal("no UpdateSubscription call recorded")
		}
		last := api.updates[len(api.updates)-1]
		got := last.Metadata["games"]
		if got != "magic,pokemon" {
			t.Errorf("games metadata = %q, want %q (pokemon dropped)", got, "magic,pokemon")
		}
		for _, item := range last.Items {
			if item.Deleted != nil && *item.Deleted {
				t.Errorf("item %s deleted; the extra_game item should survive", stripe.StringValue(item.ID))
			}
		}
	})

	t.Run("games given still changes", func(t *testing.T) {
		api, store, _ := newFixture(t, "sub_2")
		d := billingTestDeps(api, store)
		var out, errb bytes.Buffer
		code := runBilling(context.Background(), d, "plan", []string{"change", "-email", "x@example.com", "-package", "all_data", "-games", "pokemon"}, &out, &errb)
		if code != 0 {
			t.Fatalf("exit %d, stderr %q", code, errb.String())
		}
		got := api.updates[len(api.updates)-1].Metadata["games"]
		if got != "pokemon" {
			t.Errorf("games metadata = %q, want %q", got, "pokemon")
		}
	})

	t.Run("stores cleared from starter to all_data without -stores", func(t *testing.T) {
		starterKey := apiproductlist.LookupKey("starter", "monthly")
		api := newFakeBillingAPI()
		api.addPrice(starterKey)
		api.addPrice(pkgKey)
		account := apiaccess.Account{ID: 1, Email: "x@example.com", Status: "active"}
		api.addSub(t, "sub_3", map[string]string{
			"package": "starter", "interval": "monthly", "games": "magic", "stores": "cardkingdom", "account_id": "1",
		}, map[string]int64{starterKey: 1})
		store := newFakeBillingStore()
		store.addAccount(account)
		store.entitlements[1] = []apiaccess.Entitlement{{Source: "stripe", Status: "active", ExternalRef: "sub_3"}}
		d := billingTestDeps(api, store)

		var out, errb bytes.Buffer
		code := runBilling(context.Background(), d, "plan", []string{"change", "-email", "x@example.com", "-package", "all_data"}, &out, &errb)
		if code != 0 {
			t.Fatalf("exit %d, stderr %q", code, errb.String())
		}
		got := api.updates[len(api.updates)-1].Metadata["stores"]
		if got != "" {
			t.Errorf("stores metadata = %q, want empty (all_data takes no store list)", got)
		}
	})
}

// TestCheckoutLinkPrintsURLOnly is issue #33 item 9: checkout link must
// print the Checkout Session URL alone, not the billing.Session struct.
func TestCheckoutLinkPrintsURLOnly(t *testing.T) {
	api := newFakeBillingAPI()
	api.addPrice(apiproductlist.LookupKey("all_data", "monthly"))
	store := newFakeBillingStore()
	store.addAccount(apiaccess.Account{ID: 1, Email: "x@example.com", Status: "active", StripeCustomerID: "cus_1"})
	d := billingTestDeps(api, store)

	var out, errb bytes.Buffer
	code := runBilling(context.Background(), d, "checkout", []string{"link", "-email", "x@example.com", "-package", "all_data"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if len(api.updates) != 0 {
		t.Fatalf("unexpected UpdateSubscription calls: %v", api.updates)
	}
	if len(api.sessions) != 1 {
		t.Fatalf("got %d checkout sessions, want 1", len(api.sessions))
	}
	want := fmt.Sprintf("checkout link for x@example.com, valid 24 hours:\n\n    %s\n\n", api.sessions[0].URL)
	if out.String() != want {
		t.Errorf("stdout = %q, want %q", out.String(), want)
	}
}
