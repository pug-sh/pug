package dodo

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
)

// metadataOrgID is the attribution key pug sets on every checkout it starts.
const metadataOrgID = "org_id"

// Every subscription.* delivery runs one apply path and takes its new state from
// the payload's status, not from the event name -- so pausing drops the org to
// whatever is beneath the subscription and unpausing restores it, with no
// event-name branch. The prefix is therefore the whole match.
const subscriptionPrefix = "subscription."

// envelope decodes only what pug consumes. The schema is the provider's and
// evolves without us, so anything not named here is ignored rather than rejected.
type envelope struct {
	Data json.RawMessage `json:"data"`
	Type string          `json:"type"`
}

// subscriptionPayload is the subscription object as both a delivery and a direct
// read carry it, so Normalize and FetchSubscription produce identical events.
type subscriptionPayload struct {
	Currency   string `json:"currency"`
	CustomerID string `json:"-"`
	Customer   struct {
		CustomerID string `json:"customer_id"`
	} `json:"customer"`
	Metadata              map[string]string `json:"metadata"`
	NextBillingDate       *time.Time        `json:"next_billing_date"`
	PreviousBillingDate   *time.Time        `json:"previous_billing_date"`
	ProductID             string            `json:"product_id"`
	RecurringPreTaxAmount int64             `json:"recurring_pre_tax_amount"`
	Status                string            `json:"status"`
	SubscriptionID        string            `json:"subscription_id"`
}

func envelopeType(raw []byte) (string, error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", fmt.Errorf("dodo: decode webhook envelope: %w", err)
	}
	return env.Type, nil
}

// Normalize maps one verified delivery onto pug's vocabulary. A zero event means
// "store, mark processed, ignore" -- payments, refunds, disputes, and every event
// type Dodo adds after this was written. A type pug does not handle must never
// 500 and never retry forever.
func (c *Client) Normalize(d corebilling.Delivery) (corebilling.SubscriptionEvent, error) {
	if !strings.HasPrefix(d.EventType, subscriptionPrefix) {
		return corebilling.SubscriptionEvent{}, nil
	}

	var env envelope
	if err := json.Unmarshal(d.RawPayload, &env); err != nil {
		return corebilling.SubscriptionEvent{}, fmt.Errorf("dodo: decode webhook envelope: %w", err)
	}
	var payload subscriptionPayload
	if err := json.Unmarshal(env.Data, &payload); err != nil {
		return corebilling.SubscriptionEvent{}, fmt.Errorf("dodo: decode subscription payload: %w", err)
	}
	if payload.CustomerID == "" {
		payload.CustomerID = payload.Customer.CustomerID
	}
	return c.eventFromSubscription(payload), nil
}

func (c *Client) eventFromSubscription(p subscriptionPayload) corebilling.SubscriptionEvent {
	if p.CustomerID == "" {
		p.CustomerID = p.Customer.CustomerID
	}
	event := corebilling.SubscriptionEvent{
		Currency:           strings.ToUpper(strings.TrimSpace(p.Currency)),
		OrgID:              p.Metadata[metadataOrgID],
		PriceCents:         p.RecurringPreTaxAmount,
		ProductID:          p.ProductID,
		ProviderCustomerID: p.CustomerID,
		ProviderStatus:     p.Status,
		ProviderSubID:      p.SubscriptionID,
		Status:             statusFromDodo(p.Status),
	}
	if p.PreviousBillingDate != nil {
		event.CurrentPeriodStart = *p.PreviousBillingDate
	}
	if p.NextBillingDate != nil {
		event.CurrentPeriodEnd = *p.NextBillingDate
	}
	return event
}

// SlugForProduct maps a product back to a catalog tier. Not on the interface:
// the caller passes it the org's own product id for a negotiated deal, which
// this map has no entry for.
func (c *Client) SlugForProduct(productID string) (string, bool) {
	slug, ok := c.slugByProdM[productID]
	return slug, ok
}

// statusFromDodo is the first implementation of pug's vocabulary. An unmapped
// state -- Dodo's `pending`, or anything it adds later -- comes back as the
// provider's own word, which is stored verbatim and does not parse at read time.
// It is therefore not live: it can only ever withhold a plan, never grant one,
// which is what makes this mapping safe to be incomplete on its first day.
func statusFromDodo(status string) corebilling.SubStatus {
	raw := strings.ToLower(strings.TrimSpace(status))
	switch raw {
	case "active":
		return corebilling.SubStatusActive
	// The card failed, the entitlement does not.
	case "on_hold":
		return corebilling.SubStatusPastDue
	case "paused":
		return corebilling.SubStatusPaused
	case "cancelled":
		return corebilling.SubStatusCancelled
	case "expired":
		return corebilling.SubStatusExpired
	case "failed":
		return corebilling.SubStatusFailed
	}
	return corebilling.SubStatus(raw)
}
