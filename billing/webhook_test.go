package billing

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

	"github.com/stripe/stripe-go/v84"
	"github.com/stripe/stripe-go/v84/webhook"
)

const whSecret = "whsec_test"

type memLedger struct {
	rows      map[string]bool // id -> processed
	failBegin error
}

func newMemLedger() *memLedger { return &memLedger{rows: map[string]bool{}} }

func (l *memLedger) BeginStripeEvent(_ context.Context, id, _ string) (bool, error) {
	if l.failBegin != nil {
		return false, l.failBegin
	}
	if _, ok := l.rows[id]; ok {
		return false, nil
	}
	l.rows[id] = false
	return true, nil
}

func (l *memLedger) FinishStripeEvent(_ context.Context, id string) error {
	l.rows[id] = true
	return nil
}

func (l *memLedger) DeleteStripeEvent(_ context.Context, id string) error {
	delete(l.rows, id)
	return nil
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

func serve(h *Webhook, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestWebhookRejectsBadSignature(t *testing.T) {
	l, rc := newMemLedger(), &recorder{}
	h := &Webhook{Secret: whSecret, Ledger: l, Reconcile: rc.reconcile}
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
	if len(rc.ids) != 0 || len(l.rows) != 0 {
		t.Error("a rejected request reached reconcile or the ledger")
	}
}

func TestWebhookReconcilesOncePerEvent(t *testing.T) {
	l, rc := newMemLedger(), &recorder{}
	h := &Webhook{Secret: whSecret, Ledger: l, Reconcile: rc.reconcile}
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
	if processed, ok := l.rows["evt_1"]; !ok || !processed {
		t.Errorf("ledger %v", l.rows)
	}
}

func TestWebhookIgnoresOtherTypes(t *testing.T) {
	l, rc := newMemLedger(), &recorder{}
	h := &Webhook{Secret: whSecret, Ledger: l, Reconcile: rc.reconcile}
	body := eventJSON("evt_1", "customer.created", `{"object":"customer","id":"cus_1"}`)
	if rec := serve(h, signedRequest(body, whSecret)); rec.Code != http.StatusOK {
		t.Errorf("ignored type: %d", rec.Code)
	}
	if len(rc.ids) != 0 || len(l.rows) != 0 {
		t.Error("ignored type was recorded")
	}
}

func TestWebhookEventWithoutSubscription(t *testing.T) {
	l, rc := newMemLedger(), &recorder{}
	h := &Webhook{Secret: whSecret, Ledger: l, Reconcile: rc.reconcile}
	body := eventJSON("evt_1", "invoice.paid", `{"object":"invoice","id":"in_1"}`)
	if rec := serve(h, signedRequest(body, whSecret)); rec.Code != http.StatusOK {
		t.Errorf("no subscription: %d", rec.Code)
	}
	if len(rc.ids) != 0 || !l.rows["evt_1"] {
		t.Errorf("reconciled %v ledger %v", rc.ids, l.rows)
	}
}

func TestWebhookReconcileFailureIsRetryable(t *testing.T) {
	l, rc := newMemLedger(), &recorder{err: errors.New("db down")}
	h := &Webhook{Secret: whSecret, Ledger: l, Reconcile: rc.reconcile}
	body := eventJSON("evt_1", "checkout.session.completed", `{"object":"checkout.session","id":"cs_1","subscription":"sub_9"}`)
	if rec := serve(h, signedRequest(body, whSecret)); rec.Code != http.StatusInternalServerError {
		t.Errorf("failure: %d", rec.Code)
	}
	if _, ok := l.rows["evt_1"]; ok {
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
	l, rc := newMemLedger(), &recorder{}
	l.failBegin = errors.New("db down")
	h := &Webhook{Secret: whSecret, Ledger: l, Reconcile: rc.reconcile}
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
		if got := SubscriptionID(event); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
	if got := SubscriptionID(stripe.Event{}); got != "" {
		t.Errorf("empty event: %q", got)
	}
}
