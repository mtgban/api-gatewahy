package billing_test

import (
	"bytes"
	"context"
	"errors"
	"log"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/api-gatewahy/apiaccess/apiaccesstest"
	"github.com/mtgban/api-gatewahy/billing"
	"github.com/mtgban/api-gatewahy/billing/billingtest"
	"github.com/mtgban/mtgban-website/apiproductlist"
	"github.com/stripe/stripe-go/v84"
)

// seededFake is a fake Stripe holding every catalog price.
func seededFake(t *testing.T) *billingtest.FakeAPI {
	t.Helper()
	return billingtest.SeededFakeAPI(t, testCatalog)
}

// testAccount is the first account newStore creates, so its id is 1.
var testAccount = apiaccess.Account{ID: 1, Email: "ck@example.com", Status: apiaccess.AccountActive}

// testAccountRef is testAccount's id as Stripe metadata carries it.
var testAccountRef = strconv.FormatInt(testAccount.ID, 10)

// checkoutNow is the clock the checkout, the reconciler and the store share.
var checkoutNow = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

// newStore is an in-memory store holding testAccount.
func newStore(t *testing.T) *apiaccesstest.MemStore {
	t.Helper()
	s := apiaccesstest.New()
	s.Now = func() time.Time { return checkoutNow }
	if a, err := s.CreateAccount(context.Background(), testAccount.Email, ""); err != nil || a.ID != testAccount.ID {
		t.Fatalf("test account %+v %v", a, err)
	}
	return s
}

// storedAccount reads testAccount back with whatever the code under test stored on it.
func storedAccount(t *testing.T, s *apiaccesstest.MemStore) apiaccess.Account {
	t.Helper()
	a, err := s.GetAccount(context.Background(), testAccount.ID)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// invite mints an invite that lives a day on the store clock and returns its token.
func invite(t *testing.T, s *apiaccesstest.MemStore, intervalKey, email string) string {
	t.Helper()
	token, _, err := s.CreateInvite(context.Background(), intervalKey, email, 24*time.Hour, "")
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// inviteUsed reports whether the invite behind token is spent.
func inviteUsed(t *testing.T, s *apiaccesstest.MemStore, token string) bool {
	t.Helper()
	inv, ok := s.InviteByToken(token)
	if !ok {
		t.Fatalf("no invite for %q", token)
	}
	return inv.UsedAt != nil
}

func newTestCheckout(f *billingtest.FakeAPI, s *apiaccesstest.MemStore) *billing.Checkout {
	return &billing.Checkout{
		Store: s, API: f, Catalog: testCatalog, Stores: billingtest.NewFakeStores(), Games: testGames,
		SuccessURL: "https://api.mtgban.com/checkout/success", CancelURL: "https://api.mtgban.com/checkout/cancel",
		Now: func() time.Time { return checkoutNow },
	}
}

func TestCheckoutCreatesSessionAndCustomer(t *testing.T) {
	f := seededFake(t)
	s := newStore(t)
	co := newTestCheckout(f, s)
	ctx := context.Background()

	sess, err := co.Create(ctx, billing.Request{Account: testAccount, Plan: billing.Plan{Package: "starter", Interval: "monthly", Games: []string{"magic", "pokemon"}, Stores: []string{"cardkingdom", "starcitygames"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sess.URL, "https://checkout.stripe.test/") || sess.ID == "" {
		t.Errorf("session %+v", sess)
	}
	if len(f.Sessions) != 1 {
		t.Fatalf("sessions %d", len(f.Sessions))
	}
	p := f.Sessions[0]
	if stripe.StringValue(p.Mode) != "subscription" || !stripe.BoolValue(p.AllowPromotionCodes) ||
		stripe.StringValue(p.ClientReferenceID) != testAccountRef || stripe.StringValue(p.SuccessURL) != co.SuccessURL || stripe.StringValue(p.CancelURL) != co.CancelURL {
		t.Errorf("session params %+v", p)
	}
	stored := storedAccount(t, s)
	if stripe.StringValue(p.Customer) != stored.StripeCustomerID || stored.StripeCustomerID == "" {
		t.Errorf("customer %q stored %q", stripe.StringValue(p.Customer), stored.StripeCustomerID)
	}
	if cust := f.Customers[stored.StripeCustomerID]; cust == nil || cust.Email != "ck@example.com" || cust.Metadata["account_id"] != testAccountRef {
		t.Errorf("customer %+v", cust)
	}
	want := map[string]int64{f.PriceByKey("starter_monthly").ID: 1, f.PriceByKey("extra_store_monthly").ID: 1, f.PriceByKey("extra_game_monthly").ID: 1}
	if len(p.LineItems) != 3 {
		t.Fatalf("line items %d", len(p.LineItems))
	}
	for _, li := range p.LineItems {
		if want[stripe.StringValue(li.Price)] != stripe.Int64Value(li.Quantity) {
			t.Errorf("line item %s x%d", stripe.StringValue(li.Price), stripe.Int64Value(li.Quantity))
		}
	}
	md := p.SubscriptionData.Metadata
	if md["package"] != "starter" || md["interval"] != "monthly" || md["games"] != "magic,pokemon" || md["stores"] != "cardkingdom,starcitygames" || md["account_id"] != testAccountRef {
		t.Errorf("metadata %v", md)
	}

	// A second checkout reuses the stored customer.
	if _, err := co.Create(ctx, billing.Request{Account: stored, Plan: billing.Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}}); err != nil {
		t.Fatal(err)
	}
	if f.Calls["CreateCustomer"] != 1 {
		t.Errorf("CreateCustomer called %d times", f.Calls["CreateCustomer"])
	}
}

func TestCheckoutRejectsBadPlans(t *testing.T) {
	f := seededFake(t)
	co := newTestCheckout(f, newStore(t))
	ctx := context.Background()
	if _, err := co.Create(ctx, billing.Request{Account: testAccount, Plan: billing.Plan{Package: "all_data", Interval: "quarterly", Games: []string{"magic"}}}); !errors.Is(err, billing.ErrInviteRequired) {
		t.Errorf("quarterly without invite: %v", err)
	}
	if _, err := co.Create(ctx, billing.Request{Account: testAccount, Plan: billing.Plan{Package: "starter", Interval: "monthly", Games: []string{"magic"}}}); err == nil {
		t.Error("starter without stores accepted")
	}
	if _, err := co.Create(ctx, billing.Request{Account: testAccount, Plan: billing.Plan{Package: "all_data", Interval: "monthly", Games: []string{"yugioh"}}}); err == nil {
		t.Error("unknown game accepted")
	}
	suspended := testAccount
	suspended.Status = apiaccess.AccountSuspended
	if _, err := co.Create(ctx, billing.Request{Account: suspended, Plan: billing.Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}}); err == nil || !strings.Contains(err.Error(), "suspended") {
		t.Errorf("suspended account: %v", err)
	}
	if len(f.Sessions) != 0 || f.Calls["CreateCustomer"] != 0 {
		t.Error("a rejected plan reached Stripe")
	}
}

func TestCheckoutInvites(t *testing.T) {
	f := seededFake(t)
	s := newStore(t)
	co := newTestCheckout(f, s)
	ctx := context.Background()
	quarterly := billing.Plan{Package: "all_stores", Interval: "quarterly", Games: []string{"magic"}}

	if _, err := co.Create(ctx, billing.Request{Account: testAccount, Plan: quarterly, Invite: "nope"}); !errors.Is(err, apiaccess.ErrInviteInvalid) {
		t.Errorf("unknown invite: %v", err)
	}

	bound := invite(t, s, "quarterly", "someone@else.com")
	if _, err := co.Create(ctx, billing.Request{Account: testAccount, Plan: quarterly, Invite: bound}); !errors.Is(err, apiaccess.ErrInviteInvalid) {
		t.Errorf("invite bound to another email: %v", err)
	}

	wrongInterval := invite(t, s, "annual", "")
	if _, err := co.Create(ctx, billing.Request{Account: testAccount, Plan: quarterly, Invite: wrongInterval}); err == nil || !strings.Contains(err.Error(), "annual") {
		t.Errorf("invite for another interval: %v", err)
	}
	if inviteUsed(t, s, wrongInterval) {
		t.Error("mismatched invite was not released")
	}

	good := invite(t, s, "quarterly", "CK@example.com")
	f.Fail["CreateCheckoutSession"] = errors.New("stripe down")
	if _, err := co.Create(ctx, billing.Request{Account: testAccount, Plan: quarterly, Invite: good}); err == nil {
		t.Error("stripe failure swallowed")
	}
	if inviteUsed(t, s, good) {
		t.Error("invite not released after a failed session")
	}
	delete(f.Fail, "CreateCheckoutSession")
	stored := storedAccount(t, s)
	if _, err := co.Create(ctx, billing.Request{Account: stored, Plan: quarterly, Invite: good}); err != nil {
		t.Fatalf("retry with released invite: %v", err)
	}
	if !inviteUsed(t, s, good) {
		t.Error("invite not consumed")
	}
	if _, err := co.Create(ctx, billing.Request{Account: stored, Plan: quarterly, Invite: good}); !errors.Is(err, apiaccess.ErrInviteInvalid) {
		t.Errorf("invite replayed: %v", err)
	}
	if len(f.Sessions) != 1 || stripe.StringValue(f.Sessions[0].LineItems[0].Price) != f.PriceByKey("all_stores_quarterly").ID {
		t.Errorf("sessions %+v", f.Sessions)
	}
}

func TestCheckoutNeedsSeededPrices(t *testing.T) {
	f := billingtest.NewFakeAPI()
	co := newTestCheckout(f, newStore(t))
	_, err := co.Create(context.Background(), billing.Request{Account: testAccount, Plan: billing.Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}})
	if !errors.Is(err, billing.ErrPriceNotSeeded) {
		t.Errorf("unseeded: %v", err)
	}
	if f.Calls["CreateCustomer"] != 0 {
		t.Error("customer created before the line items were resolved")
	}
}

func TestCheckoutEveryPackage(t *testing.T) {
	f := seededFake(t)
	co := newTestCheckout(f, newStore(t))
	for _, pkg := range testCatalog.Packages {
		plan := billing.Plan{Package: pkg.Key, Interval: "monthly", Games: []string{"magic"}}
		if pkg.StoreScope == apiproductlist.StoreScopeExplicit {
			plan.Stores = []string{"cardkingdom"}
		}
		if _, err := co.Create(context.Background(), billing.Request{Account: testAccount, Plan: plan}); err != nil {
			t.Errorf("%s: %v", pkg.Key, err)
		}
	}
}

func TestAbandonReleasesInviteOnlyWhenStripeExpiresTheSession(t *testing.T) {
	f := seededFake(t)
	s := newStore(t)
	co := newTestCheckout(f, s)
	ctx := context.Background()
	quarterly := billing.Plan{Package: "all_stores", Interval: "quarterly", Games: []string{"magic"}}

	walkAway := invite(t, s, "quarterly", "")
	sess, err := co.Create(ctx, billing.Request{Account: testAccount, Plan: quarterly, Invite: walkAway})
	if err != nil {
		t.Fatal(err)
	}
	if err := co.Abandon(ctx, sess.ID, walkAway); err != nil {
		t.Fatalf("abandon: %v", err)
	}
	if f.SessionStatus[sess.ID] != stripe.CheckoutSessionStatusExpired {
		t.Errorf("session %s is %s", sess.ID, f.SessionStatus[sess.ID])
	}
	if inviteUsed(t, s, walkAway) {
		t.Error("invite not released after the session expired")
	}

	// A session the customer already paid for cannot be expired, so its invite stays spent.
	paid := invite(t, s, "quarterly", "")
	sess, err = co.Create(ctx, billing.Request{Account: testAccount, Plan: quarterly, Invite: paid})
	if err != nil {
		t.Fatal(err)
	}
	f.SessionStatus[sess.ID] = stripe.CheckoutSessionStatusComplete
	if err := co.Abandon(ctx, sess.ID, paid); err == nil {
		t.Error("abandon of a completed session succeeded")
	}
	if !inviteUsed(t, s, paid) {
		t.Error("invite released although the session completed")
	}

	// Stripe down: the invite stays spent rather than risk a double spend.
	outage := invite(t, s, "quarterly", "")
	sess, err = co.Create(ctx, billing.Request{Account: testAccount, Plan: quarterly, Invite: outage})
	if err != nil {
		t.Fatal(err)
	}
	f.Fail["ExpireCheckoutSession"] = errors.New("stripe down")
	if err := co.Abandon(ctx, sess.ID, outage); err == nil {
		t.Error("stripe failure swallowed")
	}
	if !inviteUsed(t, s, outage) {
		t.Error("invite released although Stripe did not confirm the expiry")
	}
}

func TestCheckoutResolvesStoresBeforeStripe(t *testing.T) {
	f := seededFake(t)
	s := newStore(t)
	co := newTestCheckout(f, s)
	lister := billingtest.NewFakeStores()
	co.Stores = lister
	ctx := context.Background()
	held := invite(t, s, "quarterly", "")

	unknown := billing.Plan{Package: "starter", Interval: "monthly", Games: []string{"magic"}, Stores: []string{"trollandtoad"}}
	var ve *billing.ValidationError
	if _, err := co.Create(ctx, billing.Request{Account: testAccount, Plan: unknown}); !errors.As(err, &ve) || ve.Msg != "Store trollandtoad is not available for the games you picked." {
		t.Errorf("unknown key: %v", err)
	}
	lister.Fail = errors.New("connection refused")
	down := billing.Plan{Package: "starter", Interval: "quarterly", Games: []string{"magic"}, Stores: []string{"cardkingdom"}}
	if _, err := co.Create(ctx, billing.Request{Account: testAccount, Plan: down, Invite: held}); !errors.Is(err, billing.ErrStoresUnavailable) {
		t.Errorf("site down: %v", err)
	}
	if f.Calls["CreateCustomer"] != 0 || f.Calls["CreateCheckoutSession"] != 0 || inviteUsed(t, s, held) {
		t.Errorf("stripe or the invite was touched: %v", f.Calls)
	}
}

func TestCheckoutWithOneSiteDown(t *testing.T) {
	f := seededFake(t)
	co := newTestCheckout(f, newStore(t))
	lister := billingtest.NewFakeStores()
	lister.Down = map[string]bool{"pokemon": true}
	co.Stores = lister
	plan := billing.Plan{Package: "starter", Interval: "monthly", Games: []string{"magic", "pokemon"}, Stores: []string{"cardkingdom"}}
	if _, err := co.Create(context.Background(), billing.Request{Account: testAccount, Plan: plan}); !errors.Is(err, billing.ErrStoresUnavailable) {
		t.Errorf("pokemon down: %v", err)
	}
	if f.Calls["CreateCheckoutSession"] != 0 || f.Calls["CreateCustomer"] != 0 {
		t.Errorf("stripe touched: %v", f.Calls)
	}
}

func TestCheckoutReusesTheCallersResolve(t *testing.T) {
	f := seededFake(t)
	co := newTestCheckout(f, newStore(t))
	lister := billingtest.NewFakeStores()
	co.Stores = lister
	plan, _ := billing.Plan{Package: "starter", Interval: "monthly", Games: []string{"magic"}, Stores: []string{"cardkingdom"}}.Validate(testCatalog, testGames, false)
	resolved, err := plan.Resolve(context.Background(), testCatalog, lister)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := co.Create(context.Background(), billing.Request{Account: testAccount, Plan: plan, Resolved: &resolved}); err != nil {
		t.Fatal(err)
	}
	if n := lister.TotalCalls(); n != 1 {
		t.Errorf("%d site lookups, want the caller's one", n)
	}
}

// releaseStore fails a release on a cancelled context, as Postgres does.
type releaseStore struct {
	*apiaccesstest.MemStore
	released []string
	fail     error
}

func (s *releaseStore) ReleaseInvite(ctx context.Context, token string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.fail != nil {
		return s.fail
	}
	s.released = append(s.released, token)
	return s.MemStore.ReleaseInvite(ctx, token)
}

func TestCheckoutReleasesInviteAfterTheClientLeaves(t *testing.T) {
	f := seededFake(t)
	s := &releaseStore{MemStore: newStore(t)}
	co := newTestCheckout(f, s.MemStore)
	co.Store = s
	quarterly := billing.Plan{Package: "all_stores", Interval: "quarterly", Games: []string{"magic"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.Fail["CreateCheckoutSession"] = context.Canceled

	left := invite(t, s.MemStore, "quarterly", "")
	if _, err := co.Create(ctx, billing.Request{Account: testAccount, Plan: quarterly, Invite: left}); !errors.Is(err, context.Canceled) {
		t.Fatalf("create: %v", err)
	}
	if inviteUsed(t, s.MemStore, left) || !slices.Equal(s.released, []string{left}) {
		t.Errorf("invite still spent after the client left; released %v", s.released)
	}

	var logged bytes.Buffer
	saved := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(saved) })
	s.fail = errors.New("db down")
	stuck := invite(t, s.MemStore, "quarterly", "")
	if _, err := co.Create(ctx, billing.Request{Account: testAccount, Plan: quarterly, Invite: stuck}); err == nil {
		t.Fatal("create succeeded")
	}
	if !strings.Contains(logged.String(), "db down") {
		t.Errorf("failed release not logged: %q", logged.String())
	}
}

// stuckStore holds every invite release until its context ends.
type stuckStore struct {
	*apiaccesstest.MemStore
}

func (s *stuckStore) ReleaseInvite(ctx context.Context, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestCheckoutBoundsTheInviteRelease(t *testing.T) {
	f := seededFake(t)
	s := &stuckStore{MemStore: newStore(t)}
	co := newTestCheckout(f, s.MemStore)
	co.Store = s
	t.Cleanup(billing.SetReleaseTimeout(time.Millisecond))
	stuck := invite(t, s.MemStore, "quarterly", "")
	f.Fail["CreateCheckoutSession"] = errors.New("stripe down")

	done := make(chan error, 1)
	go func() {
		_, err := co.Create(context.Background(), billing.Request{Account: testAccount, Plan: billing.Plan{Package: "all_stores", Interval: "quarterly", Games: []string{"magic"}}, Invite: stuck})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("create succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the invite release was not bounded")
	}
}

// putStripeRow writes testAccount's stripe row for sub_1 with the given status and valid_until.
func putStripeRow(t *testing.T, s *apiaccesstest.MemStore, status apiaccess.EntitlementStatus, until *time.Time) {
	t.Helper()
	e := apiaccess.Entitlement{AccountID: testAccount.ID, Source: apiaccess.SourceStripe, Games: []string{"magic"}, StoreScope: "ALL_ACCESS", Modes: []string{"retail"},
		Status: status, ValidFrom: checkoutNow.Add(-24 * time.Hour), ValidUntil: until, ExternalRef: "sub_1"}
	if _, err := s.UpsertStripeEntitlement(context.Background(), e); err != nil {
		t.Fatal(err)
	}
}

func TestCheckoutRefusesAnAccountWithAPlan(t *testing.T) {
	f := seededFake(t)
	s := newStore(t)
	co := newTestCheckout(f, s)
	ctx := context.Background()
	inv := invite(t, s, "quarterly", "")
	putStripeRow(t, s, apiaccess.EntitlementActive, nil)
	monthly := billing.Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}
	quarterly := billing.Plan{Package: "all_stores", Interval: "quarterly", Games: []string{"magic"}}

	if _, err := co.Create(ctx, billing.Request{Account: testAccount, Plan: monthly}); !errors.Is(err, billing.ErrHasPlan) {
		t.Errorf("monthly with a plan: %v", err)
	}
	if _, err := co.Create(ctx, billing.Request{Account: testAccount, Plan: quarterly, Invite: inv}); !errors.Is(err, billing.ErrHasPlan) {
		t.Errorf("quarterly with a plan: %v", err)
	}
	if inviteUsed(t, s, inv) {
		t.Error("refused checkout consumed the invite")
	}
	if len(f.Sessions) != 0 || f.Calls["CreateCustomer"] != 0 {
		t.Error("a refused checkout reached Stripe")
	}

	// Grace lapsing does not end the Stripe subscription, so a row past its
	// valid_until still blocks checkout while its status stays active.
	past := co.Now().Add(-time.Hour)
	putStripeRow(t, s, apiaccess.EntitlementActive, &past)
	if _, err := co.Create(ctx, billing.Request{Account: testAccount, Plan: monthly}); !errors.Is(err, billing.ErrHasPlan) {
		t.Errorf("lapsed-but-active stripe row allowed checkout: %v", err)
	}

	// Only an ended row frees the account to buy a new plan.
	putStripeRow(t, s, apiaccess.EntitlementEnded, &past)
	if _, err := co.Create(ctx, billing.Request{Account: testAccount, Plan: monthly}); err != nil {
		t.Errorf("ended stripe row blocked checkout: %v", err)
	}
}

func TestCheckoutExpiresTheCustomersOtherSessions(t *testing.T) {
	f := seededFake(t)
	s := newStore(t)
	otherAccount, err := s.CreateAccount(context.Background(), "other@example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	co := newTestCheckout(f, s)
	ctx := context.Background()
	plan := billing.Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}

	first, err := co.Create(ctx, billing.Request{Account: testAccount, Plan: plan})
	if err != nil {
		t.Fatal(err)
	}
	if f.Calls["ListOpenCheckoutSessions"] != 0 {
		t.Errorf("a new customer listed sessions %d times", f.Calls["ListOpenCheckoutSessions"])
	}
	other, err := co.Create(ctx, billing.Request{Account: otherAccount, Plan: plan})
	if err != nil {
		t.Fatal(err)
	}
	second, err := co.Create(ctx, billing.Request{Account: storedAccount(t, s), Plan: plan})
	if err != nil {
		t.Fatal(err)
	}
	if f.SessionStatus[other.ID] != stripe.CheckoutSessionStatusOpen {
		t.Errorf("another customer's session %s", f.SessionStatus[other.ID])
	}
	if f.SessionStatus[first.ID] != stripe.CheckoutSessionStatusExpired || f.SessionStatus[second.ID] != stripe.CheckoutSessionStatusOpen {
		t.Errorf("first %s, second %s", f.SessionStatus[first.ID], f.SessionStatus[second.ID])
	}

	// An expire Stripe refuses is logged, not fatal; a failed listing fails the checkout.
	f.Fail["ExpireCheckoutSession"] = errors.New("already complete")
	if _, err := co.Create(ctx, billing.Request{Account: storedAccount(t, s), Plan: plan}); err != nil {
		t.Errorf("expire failure failed the checkout: %v", err)
	}
	f.Fail["ListOpenCheckoutSessions"] = errors.New("stripe down")
	if _, err := co.Create(ctx, billing.Request{Account: storedAccount(t, s), Plan: plan}); err == nil {
		t.Error("list failure swallowed")
	}
	if len(f.Sessions) != 4 {
		t.Errorf("sessions %d", len(f.Sessions))
	}
}
