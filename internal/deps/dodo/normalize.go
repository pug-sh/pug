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

const (
	metadataInvoiceID   = "invoice_id"
	metadataPeriodStart = "period_start"
)

// Every subscription.* delivery runs one apply path and takes its state from the
// payload's status, not the event name, so pausing needs no branch of its own.
const subscriptionPrefix = "subscription."

const (
	eventPaymentMethodUpdated = "subscription.update_payment_method"
	eventPaymentSucceeded     = "payment.succeeded"
	eventPaymentFailed        = "payment.failed"
	eventRefundSucceeded      = "refund.succeeded"
)

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

// paymentPayload is the payment object as both a delivery and a direct read carry
// it, so NormalizePayment and FetchPayment produce identical payments.
type paymentPayload struct {
	CreatedAt      time.Time `json:"created_at"`
	Currency       string    `json:"currency"`
	ErrorCode      string    `json:"error_code"`
	ErrorMessage   string    `json:"error_message"`
	InvoiceURL     string    `json:"invoice_url"`
	Metadata       metadata  `json:"metadata"`
	PaymentID      string    `json:"payment_id"`
	Status         string    `json:"status"`
	SubscriptionID string    `json:"subscription_id"`
	Tax            int64     `json:"tax"`
	TotalAmount    int64     `json:"total_amount"`
}

type refundPayload struct {
	Amount    int64  `json:"amount"`
	IsPartial *bool  `json:"is_partial"`
	PaymentID string `json:"payment_id"`
	RefundID  string `json:"refund_id"`
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

// Normalize maps one verified delivery onto pug's vocabulary. A zero event is not a
// subscription's, and goes on to NormalizePayment: a type neither handles must never 500.
func (c *Client) Normalize(d corebilling.Delivery) (corebilling.SubscriptionEvent, error) {
	if !strings.HasPrefix(d.EventType, subscriptionPrefix) {
		return corebilling.SubscriptionEvent{}, nil
	}

	var payload subscriptionPayload
	if err := decodeData(d.RawPayload, &payload); err != nil {
		return corebilling.SubscriptionEvent{}, err
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
	event.PaymentMethodUpdated = d.EventType == eventPaymentMethodUpdated
	return event, nil
}

// NormalizePayment maps the payment and refund deliveries that settle an invoice.
// Every other type is a zero event, payment.cancelled included: the poll reads a
// cancelled charge as failed.
func (c *Client) NormalizePayment(d corebilling.Delivery) (corebilling.PaymentEvent, error) {
	switch d.EventType {
	case eventPaymentSucceeded, eventPaymentFailed:
		var p paymentPayload
		if err := decodeData(d.RawPayload, &p); err != nil {
			return corebilling.PaymentEvent{}, err
		}
		if p.PaymentID == "" {
			return corebilling.PaymentEvent{}, errors.New("dodo: payment payload carries no payment id")
		}
		payment := paymentFrom(p)
		// A body that disagrees with its type would otherwise be consumed as still
		// processing, and nothing reads a reopened invoice's late success again.
		want := corebilling.PaymentSucceeded
		if d.EventType == eventPaymentFailed {
			want = corebilling.PaymentFailed
		}
		if payment.Status != want {
			return corebilling.PaymentEvent{}, fmt.Errorf("dodo: %s payload carries status %q", d.EventType, p.Status)
		}
		return corebilling.PaymentEvent{Payment: payment}, nil
	case eventRefundSucceeded:
		var rf refundPayload
		if err := decodeData(d.RawPayload, &rf); err != nil {
			return corebilling.PaymentEvent{}, err
		}
		// Absent is not false: a refund read as full would mark a partial one refunded.
		if rf.PaymentID == "" || rf.RefundID == "" || rf.IsPartial == nil {
			return corebilling.PaymentEvent{}, errors.New("dodo: refund payload carries no payment id, refund id or is_partial")
		}
		return corebilling.PaymentEvent{
			Payment:       corebilling.Payment{PaymentID: rf.PaymentID},
			PartialRefund: *rf.IsPartial,
			RefundCents:   rf.Amount,
			RefundID:      rf.RefundID,
		}, nil
	}
	return corebilling.PaymentEvent{}, nil
}

func decodeData(raw []byte, into any) error {
	env, err := decodeEnvelope(raw)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(env.Data, into); err != nil {
		return fmt.Errorf("dodo: decode %s payload: %w", env.Type, err)
	}
	return nil
}

func paymentFrom(p paymentPayload) corebilling.Payment {
	return corebilling.Payment{
		CreatedAt:     p.CreatedAt,
		Currency:      strings.ToUpper(strings.TrimSpace(p.Currency)),
		ErrorCode:     p.ErrorCode,
		ErrorMessage:  p.ErrorMessage,
		InvoiceID:     p.Metadata[metadataInvoiceID],
		InvoiceURL:    p.InvoiceURL,
		PaymentID:     p.PaymentID,
		ProviderSubID: p.SubscriptionID,
		Status:        paymentStatus(p.Status),
		TaxCents:      p.Tax,
		TotalCents:    p.TotalAmount,
	}
}

// paymentStatus narrows Dodo's intent states to an outcome. Anything else, requires_*
// included, is not one yet: it delays a settle, possibly for good, and never invents one.
func paymentStatus(status string) corebilling.PaymentStatus {
	switch {
	case strings.EqualFold(strings.TrimSpace(status), "succeeded"):
		return corebilling.PaymentSucceeded
	case terminalIntent(status):
		return corebilling.PaymentFailed
	}
	return corebilling.PaymentProcessing
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
