package billing_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mtgban/api-gatewahy/apiaccess/apiaccesstest"
	"github.com/mtgban/api-gatewahy/billing"
	"github.com/mtgban/api-gatewahy/billing/billingtest"
	"github.com/stripe/stripe-go/v84"
	"github.com/stripe/stripe-go/v84/webhook"
)

const whSecret = "whsec_test"

var _ billing.Ledger = (*apiaccesstest.MemStore)(nil)

// processed reports whether the ledger finished id: a claim that is merely
// in flight can be taken again once it is stale, a finished one never.
func processed(t *testing.T, l *apiaccesstest.MemStore, id string) bool {
	t.Helper()
	l.Now = func() time.Time { return time.Now().Add(time.Hour) }
	defer func() { l.Now = time.Now }()
	claimed, err := l.BeginStripeEvent(context.Background(), id, "")
	if err != nil {
		t.Fatal(err)
	}
	return !claimed
}

func eventJSON(id, typ, object string) string {
	return fmt.Sprintf(`{"id":%q,"object":"event","type":%q,"data":{"object":%s}}`, id, typ, object)
}

func signedRequest(body, secret string) *http.Request {
	sp := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: []byte(body), Secret: secret, Timestamp: time.Now()})
	req := httptest.NewRequest(http.MethodPost, "/stripe/webhook", bytes.NewReader(sp.Payload))
	req.Header.Set("Stripe-Signature", sp.Header)
	return req
}

type recorder struct {
	ids []string
	err error
}

func (r *recorder) reconcile(_ context.Context, id string) error {
	r.ids = append(r.ids, id)
	return r.err
}

func serve(h *billing.Webhook, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestWebhookRejectsBadSignature(t *testing.T) {
	l, rc := apiaccesstest.New(), &recorder{}
	h := &billing.Webhook{Secret: whSecret, Ledger: l, Reconcile: rc.reconcile}
	body := eventJSON("evt_1", "customer.subscription.updated", `{"object":"subscription","id":"sub_1"}`)
	if rec := serve(h, signedRequest(body, "whsec_other")); rec.Code != http.StatusBadRequest {
		t.Errorf("wrong secret: %d", rec.Code)
	}
	unsigned := httptest.NewRequest(http.MethodPost, "/stripe/webhook", bytes.NewReader([]byte(body)))
	if rec := serve(h, unsigned); rec.Code != http.StatusBadRequest {
		t.Errorf("no signature: %d", rec.Code)
	}
	if rec := serve(h, httptest.NewRequest(http.MethodGet, "/stripe/webhook", nil)); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", rec.Code)
	}
	if len(rc.ids) != 0 || l.Calls["BeginStripeEvent"] != 0 {
		t.Error("a rejected request reached reconcile or the ledger")
	}
}

func TestWebhookReconcilesOncePerEvent(t *testing.T) {
	l, rc := apiaccesstest.New(), &recorder{}
	h := &billing.Webhook{Secret: whSecret, Ledger: l, Reconcile: rc.reconcile}
	body := eventJSON("evt_1", "customer.subscription.updated", `{"object":"subscription","id":"sub_1"}`)
	if rec := serve(h, signedRequest(body, whSecret)); rec.Code != http.StatusOK {
		t.Fatalf("first delivery: %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(h, signedRequest(body, whSecret)); rec.Code != http.StatusOK {
		t.Fatalf("redelivery: %d", rec.Code)
	}
	if len(rc.ids) != 1 || rc.ids[0] != "sub_1" {
		t.Errorf("reconciled %v", rc.ids)
	}
	if !processed(t, l, "evt_1") {
		t.Errorf("evt_1 not finished in the ledger: %v", l.Calls)
	}
}

func TestWebhookIgnoresOtherTypes(t *testing.T) {
	l, rc := apiaccesstest.New(), &recorder{}
	h := &billing.Webhook{Secret: whSecret, Ledger: l, Reconcile: rc.reconcile}
	body := eventJSON("evt_1", "customer.created", `{"object":"customer","id":"cus_1"}`)
	if rec := serve(h, signedRequest(body, whSecret)); rec.Code != http.StatusOK {
		t.Errorf("ignored type: %d", rec.Code)
	}
	if len(rc.ids) != 0 || l.Calls["BeginStripeEvent"] != 0 {
		t.Error("ignored type was recorded")
	}
}

func TestWebhookEventWithoutSubscription(t *testing.T) {
	l, rc := apiaccesstest.New(), &recorder{}
	h := &billing.Webhook{Secret: whSecret, Ledger: l, Reconcile: rc.reconcile}
	body := eventJSON("evt_1", "invoice.paid", `{"object":"invoice","id":"in_1"}`)
	if rec := serve(h, signedRequest(body, whSecret)); rec.Code != http.StatusOK {
		t.Errorf("no subscription: %d", rec.Code)
	}
	if len(rc.ids) != 0 || !processed(t, l, "evt_1") {
		t.Errorf("reconciled %v ledger calls %v", rc.ids, l.Calls)
	}
}

func TestWebhookReconcileFailureIsRetryable(t *testing.T) {
	l, rc := apiaccesstest.New(), &recorder{err: errors.New("db down")}
	h := &billing.Webhook{Secret: whSecret, Ledger: l, Reconcile: rc.reconcile}
	body := eventJSON("evt_1", "checkout.session.completed", `{"object":"checkout.session","id":"cs_1","subscription":"sub_9"}`)
	if rec := serve(h, signedRequest(body, whSecret)); rec.Code != http.StatusInternalServerError {
		t.Errorf("failure: %d", rec.Code)
	}
	if l.Calls["DeleteStripeEvent"] != 1 {
		t.Error("claim kept after a failure")
	}
	rc.err = nil
	if rec := serve(h, signedRequest(body, whSecret)); rec.Code != http.StatusOK {
		t.Errorf("retry: %d", rec.Code)
	}
	if len(rc.ids) != 2 || rc.ids[1] != "sub_9" {
		t.Errorf("reconciled %v", rc.ids)
	}
}

func TestWebhookLedgerDown(t *testing.T) {
	l, rc := apiaccesstest.New(), &recorder{}
	l.Fail["BeginStripeEvent"] = errors.New("db down")
	h := &billing.Webhook{Secret: whSecret, Ledger: l, Reconcile: rc.reconcile}
	body := eventJSON("evt_1", "customer.subscription.deleted", `{"object":"subscription","id":"sub_1"}`)
	if rec := serve(h, signedRequest(body, whSecret)); rec.Code != http.StatusInternalServerError {
		t.Errorf("ledger down: %d", rec.Code)
	}
	if len(rc.ids) != 0 {
		t.Error("reconciled without a claim")
	}
}

func TestSubscriptionID(t *testing.T) {
	cases := []struct {
		name, object, want string
	}{
		{"subscription", `{"object":"subscription","id":"sub_1"}`, "sub_1"},
		{"checkout string", `{"object":"checkout.session","id":"cs_1","subscription":"sub_2"}`, "sub_2"},
		{"checkout expanded", `{"object":"checkout.session","id":"cs_1","subscription":{"id":"sub_3"}}`, "sub_3"},
		{"checkout none", `{"object":"checkout.session","id":"cs_1","subscription":null}`, ""},
		{"invoice legacy", `{"object":"invoice","id":"in_1","subscription":"sub_4"}`, "sub_4"},
		{"invoice parent", `{"object":"invoice","id":"in_1","parent":{"type":"subscription_details","subscription_details":{"subscription":"sub_5"}}}`, "sub_5"},
		{"invoice parent expanded", `{"object":"invoice","id":"in_1","parent":{"subscription_details":{"subscription":{"id":"sub_6"}}}}`, "sub_6"},
		{"invoice one-off", `{"object":"invoice","id":"in_1","parent":null}`, ""},
		{"customer", `{"object":"customer","id":"cus_1"}`, ""},
	}
	for _, c := range cases {
		var event stripe.Event
		if err := json.Unmarshal([]byte(eventJSON("evt", "x", c.object)), &event); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := billing.SubscriptionID(event); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
	if got := billing.SubscriptionID(stripe.Event{}); got != "" {
		t.Errorf("empty event: %q", got)
	}
}

// TestWebhookApplyErrors runs a real reconciler behind the webhook and
// delivers each event twice, as Stripe retries a 500.
func TestWebhookApplyErrors(t *testing.T) {
	plan, _ := billing.Plan{Package: "all_data", Interval: "monthly", Games: []string{"magic"}}.Normalize(testCatalog)
	starter, _ := billing.Plan{Package: "starter", Interval: "monthly", Games: []string{"magic"}, Stores: []string{"nosuchstore"}}.Normalize(testCatalog)
	cases := []struct {
		name      string
		metadata  map[string]string
		customer  string
		item      string
		fault     func(f *billingtest.FakeAPI, s *apiaccesstest.MemStore, r *billing.Reconciler)
		permanent bool
	}{
		{name: "no plan metadata", metadata: map[string]string{"account_id": testAccountRef}, permanent: true},
		{name: "unknown package", metadata: map[string]string{"account_id": testAccountRef, "package": "gold", "interval": "monthly"}, permanent: true},
		{name: "no account", metadata: plan.Metadata(999), customer: "cus_nobody", permanent: true},
		{name: "store not sold", metadata: starter.Metadata(testAccount.ID), item: "starter_monthly", permanent: true},
		{name: "stripe down", metadata: plan.Metadata(testAccount.ID), fault: func(f *billingtest.FakeAPI, _ *apiaccesstest.MemStore, _ *billing.Reconciler) {
			f.Fail["GetSubscription"] = &stripe.Error{Msg: "api error", HTTPStatusCode: 500}
		}},
		{name: "db down", metadata: plan.Metadata(testAccount.ID), fault: func(_ *billingtest.FakeAPI, s *apiaccesstest.MemStore, _ *billing.Reconciler) {
			s.Fail["UpsertStripeEntitlement"] = errors.New("db down")
		}},
		{name: "stores down", metadata: starter.Metadata(testAccount.ID), item: "starter_monthly", fault: func(_ *billingtest.FakeAPI, _ *apiaccesstest.MemStore, r *billing.Reconciler) {
			lister := billingtest.NewFakeStores()
			lister.Fail = errors.New("connection refused")
			r.Stores = lister
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := seededFake(t)
			s := newStore(t)
			var alerts []string
			r := newTestReconciler(f, s, &alerts)
			customer := c.customer
			if customer == "" {
				customer = "cus_x"
			}
			item := c.item
			if item == "" {
				item = "all_data_monthly"
			}
			f.AddSub(t, "sub_1", customer, stripe.SubscriptionStatusActive, c.metadata, periodEnd, billingtest.Item{Key: item, Qty: 1})
			if c.fault != nil {
				c.fault(f, s, r)
			}
			l := apiaccesstest.New()
			h := &billing.Webhook{Secret: whSecret, Ledger: l, Reconcile: r.Subscription}
			body := eventJSON("evt_1", "customer.subscription.updated", `{"object":"subscription","id":"sub_1"}`)
			first, second := serve(h, signedRequest(body, whSecret)), serve(h, signedRequest(body, whSecret))
			if c.permanent {
				if first.Code != http.StatusOK || second.Code != http.StatusOK || !processed(t, l, "evt_1") {
					t.Errorf("codes %d, %d, ledger calls %v; want 200 and the event finished", first.Code, second.Code, l.Calls)
				}
				if len(alerts) != 1 {
					t.Errorf("alerts %q, want one", alerts)
				}
				return
			}
			if first.Code != http.StatusInternalServerError || second.Code != http.StatusInternalServerError {
				t.Errorf("codes %d, %d; want 500 so Stripe retries", first.Code, second.Code)
			}
			if claimed, err := l.BeginStripeEvent(context.Background(), "evt_1", ""); err != nil || !claimed {
				t.Errorf("claim kept after a retryable failure: %v", err)
			}
		})
	}
}
