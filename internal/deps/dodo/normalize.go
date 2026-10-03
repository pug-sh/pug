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
// read carry it, so Normalize and FetchSubscription produce identical events — but
// for the grace deadline, which only a delivery carries.
type subscriptionPayload struct {
	Currency   string `json:"currency"`
	CustomerID string `json:"-"`
	Customer   struct {
		CustomerID string `json:"customer_id"`
	} `json:"customer"`
	Metadata        metadata   `json:"metadata"`
	NextBillingDate *time.Time `json:"next_billing_date"`
	// Kept raw: whether the key was sent at all matters, and a value that does not
	// parse must not fail the delivery, since the field only dates a banner.
	GracePeriodEndsAt     presentJSON `json:"past_due_ends_at"`
	PreviousBillingDate   *time.Time  `json:"previous_billing_date"`
	ProductID             string      `json:"product_id"`
	RecurringPreTaxAmount int64       `json:"recurring_pre_tax_amount"`
	Status                string      `json:"status"`
	SubscriptionID        string      `json:"subscription_id"`
}

// presentJSON keeps a field's raw value and whether its key was sent: encoding/json
// calls UnmarshalJSON for an explicit null, never for a missing key.
type presentJSON struct {
	Present bool
	Raw     json.RawMessage
}

func (p *presentJSON) UnmarshalJSON(b []byte) error {
	p.Present, p.Raw = true, append(p.Raw[:0], b...)
	return nil
}

func (p presentJSON) isNull() bool { return string(p.Raw) == "null" }

// A grace deadline is believed only where a window can end: no earlier than the
// delivery that dates it, give or take a window closing as it is sent, and within a
// year of it. Dodo's windows run 1–30 days; anything far outside is garbage.
const (
	graceDeadlineSlack   = time.Hour
	graceDeadlineHorizon = 366 * 24 * time.Hour
)

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
	// Only a delivery carries the deadline. Inside the window it is believed only when
	// it is there, parses and is plausible; otherwise it stays unknown, which keeps the
	// stored one, and the apply logs why. Outside a window there is none to believe.
	if inGraceWindow(payload.Status) {
		event.GracePeriodEndsAt, event.GracePeriodEndsAtIssue = graceDeadline(payload.GracePeriodEndsAt, d.DeliveredAt)
		event.GracePeriodEndsAtKnown = event.GracePeriodEndsAtIssue == ""
	} else if payload.GracePeriodEndsAt.Present && !payload.GracePeriodEndsAt.isNull() {
		event.GracePeriodEndsAtIssue = "past_due_ends_at sent outside a grace window, in " + payload.Status
	}
	return event, nil
}

// graceDeadline is the deadline a delivery inside a grace window dates it with, or
// why none can be believed.
func graceDeadline(field presentJSON, sent time.Time) (time.Time, string) {
	if !field.Present {
		return time.Time{}, "no past_due_ends_at inside a grace window"
	}
	if field.isNull() {
		return time.Time{}, "a null past_due_ends_at inside a grace window"
	}
	var at time.Time
	if err := json.Unmarshal(field.Raw, &at); err != nil {
		return time.Time{}, "past_due_ends_at does not parse: " + err.Error()
	}
	if at.Before(sent.Add(-graceDeadlineSlack)) || at.After(sent.Add(graceDeadlineHorizon)) {
		return time.Time{}, fmt.Sprintf("past_due_ends_at %s cannot end a window open at %s",
			at.Format(time.RFC3339), sent.Format(time.RFC3339))
	}
	return at, ""
}

// inGraceWindow is Dodo's own past_due, the one state it dates a grace window in.
// pug's past_due also holds on_hold, which has none.
func inGraceWindow(status string) bool {
	return strings.ToLower(strings.TrimSpace(status)) == "past_due"
}

func (c *Client) eventFromSubscription(p subscriptionPayload) corebilling.SubscriptionEvent {
	if p.CustomerID == "" {
		p.CustomerID = p.Customer.CustomerID
	}
	event := corebilling.SubscriptionEvent{
		CheckoutRef:        p.Metadata[metadataCheckoutRef],
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
	// Outside Dodo's past_due there is no grace window, which even a read can say:
	// otherwise a hold read after its window ended would keep a date already past.
	// Inside one only a delivery carries the date, and Normalize reads it.
	event.GracePeriodEndsAtKnown = !inGraceWindow(p.Status)
	return event
}

// An unmapped state comes back as the provider's own word, stored verbatim and
// unparsed at read time — so it can only withhold a plan, never grant one,
// provided every word that spells a live pug state is mapped here.
func statusFromDodo(status string) corebilling.SubStatus {
	raw := strings.ToLower(strings.TrimSpace(status))
	switch raw {
	case "active":
		return corebilling.SubStatusActive
	// The card failed, the entitlement does not: on_hold outside Dodo's grace
	// period, past_due inside it.
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
