package billing

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/stripe/stripe-go/v84"
	"github.com/stripe/stripe-go/v84/webhook"
)

// Ledger is the stripe_events table: one row per event id.
type Ledger interface {
	BeginStripeEvent(ctx context.Context, id, typ string) (bool, error)
	FinishStripeEvent(ctx context.Context, id string) error
	DeleteStripeEvent(ctx context.Context, id string) error
}

var _ Ledger = (*apiaccess.Client)(nil)

// handledEvents are the types the dashboard endpoint subscribes to.
var handledEvents = map[stripe.EventType]bool{
	stripe.EventTypeCheckoutSessionCompleted:    true,
	stripe.EventTypeCustomerSubscriptionCreated: true,
	stripe.EventTypeCustomerSubscriptionUpdated: true,
	stripe.EventTypeCustomerSubscriptionDeleted: true,
	stripe.EventTypeInvoicePaid:                 true,
	stripe.EventTypeInvoicePaymentFailed:        true,
}

// maxWebhookBody bounds what the handler reads; events are a few KB.
const maxWebhookBody = 1 << 20

// Webhook verifies Stripe's signature and reconciles the named subscription
// once per event id.
type Webhook struct {
	Secret    string
	Ledger    Ledger
	Reconcile func(ctx context.Context, subID string) error
}

func (h *Webhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	// Only ids are read from the payload, so a newer API version is fine.
	event, err := webhook.ConstructEventWithOptions(body, r.Header.Get("Stripe-Signature"), h.Secret,
		webhook.ConstructEventOptions{IgnoreAPIVersionMismatch: true})
	if err != nil {
		log.Printf("stripe webhook: rejected: %v", err)
		http.Error(w, "bad signature", http.StatusBadRequest)
		return
	}
	if !handledEvents[event.Type] {
		w.WriteHeader(http.StatusOK)
		return
	}
	ctx := r.Context()
	fresh, err := h.Ledger.BeginStripeEvent(ctx, event.ID, string(event.Type))
	if err != nil {
		log.Printf("stripe webhook %s: ledger: %v", event.ID, err)
		http.Error(w, "ledger unavailable", http.StatusInternalServerError)
		return
	}
	if !fresh {
		w.WriteHeader(http.StatusOK)
		return
	}
	subID := SubscriptionID(event)
	if subID == "" {
		h.finish(ctx, event.ID)
		w.WriteHeader(http.StatusOK)
		return
	}
	if err := h.Reconcile(ctx, subID); err != nil {
		log.Printf("stripe webhook %s (%s): %v", event.ID, subID, err)
		if derr := h.Ledger.DeleteStripeEvent(ctx, event.ID); derr != nil {
			log.Printf("stripe webhook %s: release claim: %v", event.ID, derr)
		}
		http.Error(w, "reconcile failed", http.StatusInternalServerError)
		return
	}
	h.finish(ctx, event.ID)
	w.WriteHeader(http.StatusOK)
}

// finish marks the event done; a failure here only means a stale claim
// that BeginStripeEvent reclaims after a few minutes.
func (h *Webhook) finish(ctx context.Context, id string) {
	if err := h.Ledger.FinishStripeEvent(ctx, id); err != nil {
		log.Printf("stripe webhook %s: finish: %v", id, err)
	}
}

// SubscriptionID finds the subscription an event is about, or "".
func SubscriptionID(event stripe.Event) string {
	if event.Data == nil || len(event.Data.Raw) == 0 {
		return ""
	}
	var obj struct {
		Object       string          `json:"object"`
		ID           string          `json:"id"`
		Subscription json.RawMessage `json:"subscription"`
		Parent       *struct {
			SubscriptionDetails *struct {
				Subscription json.RawMessage `json:"subscription"`
			} `json:"subscription_details"`
		} `json:"parent"`
	}
	if err := json.Unmarshal(event.Data.Raw, &obj); err != nil {
		return ""
	}
	switch obj.Object {
	case "subscription":
		return obj.ID
	case "checkout.session":
		return rawID(obj.Subscription)
	case "invoice":
		if id := rawID(obj.Subscription); id != "" {
			return id
		}
		if obj.Parent != nil && obj.Parent.SubscriptionDetails != nil {
			return rawID(obj.Parent.SubscriptionDetails.Subscription)
		}
	}
	return ""
}

// rawID reads an id from a bare string or an expanded object.
func rawID(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var o struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &o) == nil {
		return o.ID
	}
	return ""
}
