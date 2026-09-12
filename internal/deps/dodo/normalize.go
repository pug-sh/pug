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
// buyer set metadata_*.
const metadataOrgID = "org_id"

// metadataCheckoutRef is the one a buyer cannot forge, so attribution prefers it.
const metadataCheckoutRef = "checkout_ref"

// metadataInvoiceID rides every charge pug makes, so a payment webhook names
// the invoice it settles.
const metadataInvoiceID = "invoice_id"

const metadataPeriodStart = "period_start"

// Every subscription.* delivery runs one apply path and takes its state from the
// payload's status, not the event name, so pausing needs no branch of its own.
const subscriptionPrefix = "subscription."

const paymentPrefix = "payment."

const eventPaymentMethodUpdated = "subscription.update_payment_method"

const eventRefundSucceeded = "refund.succeeded"

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
	ExpiresAt             *time.Time `json:"expires_at"`
	Metadata              metadata   `json:"metadata"`
	NextBillingDate       *time.Time `json:"next_billing_date"`
	OnDemand              bool       `json:"on_demand"`
	PreviousBillingDate   *time.Time `json:"previous_billing_date"`
	ProductID             string     `json:"product_id"`
	RecurringPreTaxAmount int64      `json:"recurring_pre_tax_amount"`
	Status                string     `json:"status"`
	SubscriptionID        string     `json:"subscription_id"`
}

type paymentPayload struct {
	CreatedAt    *time.Time `json:"created_at"`
	Currency     string     `json:"currency"`
	ErrorCode    string     `json:"error_code"`
	ErrorMessage string     `json:"error_message"`
	InvoiceURL   string     `json:"invoice_url"`
	Metadata     metadata   `json:"metadata"`
	PaymentID    string     `json:"payment_id"`
	Status       string     `json:"status"`
	TotalAmount  int64      `json:"total_amount"`
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

// Normalize maps one verified subscription delivery onto pug's vocabulary. A
// zero event means the delivery is not a subscription event.
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
	event.PaymentMethodUpdated = d.EventType == eventPaymentMethodUpdated
	return event, nil
}

// NormalizePayment maps a payment or refund delivery. Anything else, and every
// type added after this was written, normalizes to nothing.
func (c *Client) NormalizePayment(d corebilling.Delivery) (corebilling.PaymentEvent, error) {
	refund := d.EventType == eventRefundSucceeded
	if !refund && !strings.HasPrefix(d.EventType, paymentPrefix) {
		return corebilling.PaymentEvent{}, nil
	}
	env, err := decodeEnvelope(d.RawPayload)
	if err != nil {
		return corebilling.PaymentEvent{}, err
	}
	var payload paymentPayload
	if err := json.Unmarshal(env.Data, &payload); err != nil {
		return corebilling.PaymentEvent{}, fmt.Errorf("dodo: decode payment payload: %w", err)
	}
	if payload.PaymentID == "" {
		return corebilling.PaymentEvent{}, errors.New("dodo: payment payload carries no payment id")
	}
	event := corebilling.PaymentEvent{
		Refund: refund,
		Payment: corebilling.PaymentRecord{
			PaymentID:    payload.PaymentID,
			InvoiceID:    payload.Metadata[metadataInvoiceID],
			Status:       paymentStatus(payload.Status),
			ErrorCode:    payload.ErrorCode,
			ErrorMessage: payload.ErrorMessage,
			InvoiceURL:   payload.InvoiceURL,
			AmountCents:  payload.TotalAmount,
			Currency:     strings.ToUpper(strings.TrimSpace(payload.Currency)),
		},
	}
	if payload.CreatedAt != nil {
		event.Payment.CreatedAt = *payload.CreatedAt
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
		OnDemand:           p.OnDemand,
		OrgID:              p.Metadata[metadataOrgID],
		PriceCents:         p.RecurringPreTaxAmount,
		ProductID:          p.ProductID,
		ProviderCustomerID: p.CustomerID,
		ProviderStatus:     p.Status,
		ProviderSubID:      p.SubscriptionID,
		Status:             status,
	}
	if p.PreviousBillingDate != nil {
		event.CurrentPeriodStart = *p.PreviousBillingDate
	}
	if p.NextBillingDate != nil {
		event.CurrentPeriodEnd = *p.NextBillingDate
	}
	switch {
	case p.CancelledAt != nil && !p.CancelledAt.IsZero():
		event.EndedAt = *p.CancelledAt
	case !status.Live() && p.ExpiresAt != nil && !p.ExpiresAt.IsZero():
		event.EndedAt = *p.ExpiresAt
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
	// The card failed, the entitlement does not.
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
