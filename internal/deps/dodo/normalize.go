package dodo

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
)

// metadataOrgID names an org rather than proving one: static payment links let a
// buyer set metadata_*. It counts only beside a product an operator staged.
const metadataOrgID = "org_id"

// metadataCheckoutRef is the one a buyer cannot forge, so attribution prefers it.
const metadataCheckoutRef = "checkout_ref"

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
	CancelAtNextBillingDate bool       `json:"cancel_at_next_billing_date"`
	CancelledAt             *time.Time `json:"cancelled_at"`
	Currency                string     `json:"currency"`
	CustomerID              string     `json:"-"`
	Customer                struct {
		CustomerID string `json:"customer_id"`
	} `json:"customer"`
	ExpiresAt           *time.Time `json:"expires_at"`
	Metadata            metadata   `json:"metadata"`
	NextBillingDate     *time.Time `json:"next_billing_date"`
	OnDemand            *bool      `json:"on_demand"`
	PreviousBillingDate *time.Time `json:"previous_billing_date"`
	ProductID           string     `json:"product_id"`
	Status              string     `json:"status"`
	SubscriptionID      string     `json:"subscription_id"`
	TaxInclusive        *bool      `json:"tax_inclusive"`
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

func decodeEnvelope(raw []byte) (envelope, error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return envelope{}, fmt.Errorf("dodo: decode webhook envelope: %w", err)
	}
	return env, nil
}

func envelopeType(raw []byte) (string, error) {
	env, err := decodeEnvelope(raw)
	return env.Type, err
}

// Normalize maps one verified delivery onto pug's vocabulary. A zero event means
// "store, mark processed, ignore" — a type pug does not handle must never 500.
func (c *Client) Normalize(d corebilling.Delivery) (corebilling.SubscriptionEvent, error) {
	if !strings.HasPrefix(d.EventType, subscriptionPrefix) {
		return corebilling.SubscriptionEvent{}, nil
	}

	env, err := decodeEnvelope(d.RawPayload)
	if err != nil {
		return corebilling.SubscriptionEvent{}, err
	}
	var payload subscriptionPayload
	if err := json.Unmarshal(env.Data, &payload); err != nil {
		return corebilling.SubscriptionEvent{}, fmt.Errorf("dodo: decode subscription payload: %w", err)
	}
	event := c.eventFromSubscription(payload)
	// A subscription event that yields nothing is a payload shape pug no longer
	// understands: a zero event here would store it as cleanly processed.
	if event.IsZero() {
		return corebilling.SubscriptionEvent{}, errors.New("dodo: subscription payload carries no subscription id")
	}
	// Absent is not false: a missing on_demand would refuse a real mandate for good,
	// and a missing tax_inclusive would accept one pug under-collects on.
	if payload.OnDemand == nil || payload.TaxInclusive == nil {
		return corebilling.SubscriptionEvent{}, errors.New(
			"dodo: subscription payload carries no on_demand or tax_inclusive")
	}
	return event, nil
}

func (c *Client) eventFromSubscription(p subscriptionPayload) corebilling.SubscriptionEvent {
	if p.CustomerID == "" {
		p.CustomerID = p.Customer.CustomerID
	}
	status := statusFromDodo(p.Status)
	event := corebilling.SubscriptionEvent{
		CancelAtPeriodEnd:  p.CancelAtNextBillingDate,
		CheckoutRef:        p.Metadata[metadataCheckoutRef],
		Currency:           strings.ToUpper(strings.TrimSpace(p.Currency)),
		OnDemand:           p.OnDemand != nil && *p.OnDemand,
		OrgID:              p.Metadata[metadataOrgID],
		ProductID:          p.ProductID,
		ProviderCustomerID: p.CustomerID,
		ProviderStatus:     p.Status,
		ProviderSubID:      p.SubscriptionID,
		Status:             status,
		TaxInclusive:       p.TaxInclusive != nil && *p.TaxInclusive,
	}
	if p.PreviousBillingDate != nil {
		event.CurrentPeriodStart = *p.PreviousBillingDate
	}
	if p.NextBillingDate != nil {
		event.CurrentPeriodEnd = *p.NextBillingDate
	}
	// Both are stamped on a live subscription too: expires_at as the trial's end,
	// cancelled_at as a cancellation only scheduled.
	if !status.Live() {
		switch {
		case p.CancelledAt != nil && !p.CancelledAt.IsZero():
			event.EndedAt = *p.CancelledAt
		case p.ExpiresAt != nil && !p.ExpiresAt.IsZero():
			event.EndedAt = *p.ExpiresAt
		}
	}
	return event
}

// An unmapped state comes back as the provider's own word, stored verbatim and
// unparsed at read time — so it can only withhold a plan, never grant one.
func statusFromDodo(status string) corebilling.SubStatus {
	raw := strings.ToLower(strings.TrimSpace(status))
	switch raw {
	case "active":
		return corebilling.SubStatusActive
	// The card failed, the entitlement does not. Dodo's on-demand guide is explicit
	// that on_hold does not stop a charge, so a failed invoice retries against both.
	case "on_hold", "past_due":
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
