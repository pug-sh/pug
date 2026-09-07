package dodo

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
)

// metadataOrgID is the attribution key pug sets on every checkout it starts.
const metadataOrgID = "org_id"

// Every subscription.* delivery runs one apply path and takes its state from the
// payload's status, not the event name, so pausing needs no branch of its own.
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
	Metadata              metadata   `json:"metadata"`
	NextBillingDate       *time.Time `json:"next_billing_date"`
	PreviousBillingDate   *time.Time `json:"previous_billing_date"`
	ProductID             string     `json:"product_id"`
	RecurringPreTaxAmount int64      `json:"recurring_pre_tax_amount"`
	Status                string     `json:"status"`
	SubscriptionID        string     `json:"subscription_id"`
}

// metadata narrows Dodo's string|number|bool map to the string values pug writes:
// decoding into map[string]string would fail a delivery over one numeric value.
type metadata map[string]string

func (m *metadata) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	out := make(metadata, len(raw))
	for k, v := range raw {
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			out[k] = s
		}
	}
	*m = out
	return nil
}

func envelopeType(raw []byte) (string, error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", fmt.Errorf("dodo: decode webhook envelope: %w", err)
	}
	return env.Type, nil
}

// Normalize maps one verified delivery onto pug's vocabulary. A zero event means
// "store, mark processed, ignore" -- a type pug does not handle must never 500.
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
	event := c.eventFromSubscription(payload)
	// A subscription event that yields nothing is a payload shape pug no longer
	// understands: a zero event here would store it as cleanly processed.
	if event.IsZero() {
		return corebilling.SubscriptionEvent{}, errors.New("dodo: subscription payload carries no subscription id")
	}
	return event, nil
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

// statusFromDodo is the first implementation of pug's vocabulary. An unmapped
// state comes back as the provider's own word, stored verbatim and unparsed at
// read time -- so it can only withhold a plan, never grant one.
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
