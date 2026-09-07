package billing

import (
	"context"
	"net/http"
	"time"
)

// PaymentProvider is the whole seam between pug and a merchant of record.
// Nothing else in this slice imports a provider package: resolution, the inbox,
// reconcile, the RPCs and the dashboard all operate on the types below.
//
// The payload is deliberately not abstracted. Verify hands back the bytes as
// sent, which the inbox stores, and Normalize produces one small canonical
// struct everything downstream reads. A common payload schema across providers
// is the thing that cannot be maintained.
type PaymentProvider interface {
	// Name is the provider's slug. It is stored on every row the provider
	// produces and it is the path segment its webhook is mounted at, so it is an
	// identifier rather than a label -- changing it orphans stored rows.
	Name() string

	// Verify authenticates a raw delivery. It takes the exact bytes because every
	// signature scheme signs those, not a decoded message.
	Verify(headers http.Header, rawBody []byte) (Delivery, error)

	// Normalize maps one verified delivery onto pug's vocabulary. A zero
	// SubscriptionEvent means "store, mark processed, ignore" -- the disposition
	// for a payment, a refund, and every event type that appears after this was
	// written.
	Normalize(Delivery) (SubscriptionEvent, error)

	// CreateCheckoutSession returns the session's id alongside the URL to send the
	// buyer to. The id is what ConfirmCheckout later re-reads the outcome by; a
	// provider with no such handle returns an empty one, which leaves the webhook
	// as the only confirmation path.
	CreateCheckoutSession(ctx context.Context, in CheckoutInput) (sessionID, checkoutURL string, err error)
	CreatePortalSession(ctx context.Context, customerID string) (string, error)
	// FetchSubscription re-reads one subscription for the reconcile pass, in the
	// same shape a delivery would have carried.
	FetchSubscription(ctx context.Context, providerSubID string) (SubscriptionEvent, error)
	// FetchCheckoutOutcome re-reads one checkout the dashboard started, in that
	// same shape. It is what confirms a returning buyer without waiting on a
	// delivery.
	//
	// A zero SubscriptionEvent means "not settled yet" and is not an error. A
	// checkout the provider has given up on -- a declined card, an unknown or
	// expired session -- must NOT use it: return ErrCheckoutFailed, or the buyer
	// waits out a poll for money that will never arrive.
	FetchCheckoutOutcome(ctx context.Context, sessionID string) (SubscriptionEvent, error)
}

// Delivery is one verified webhook, still in the provider's own vocabulary.
type Delivery struct {
	// WebhookID is the provider's id for this delivery. Its retry reuses it, which
	// is what makes the inbox's primary key a deduplication.
	WebhookID string
	EventType string
	// RawPayload is the body as sent. Verify reads these exact bytes; what the
	// inbox stores is jsonb, so it comes back canonicalized rather than verbatim.
	RawPayload []byte
	// DeliveredAt bounds payload freshness and is the CAS guard. It comes from the
	// signed envelope rather than from a payload field because every delivery
	// carries the latest object and this is the one stamp uniform across event
	// types.
	DeliveredAt time.Time
}

// SubscriptionEvent is a delivery in pug's vocabulary. The zero value means
// there is nothing to apply.
type SubscriptionEvent struct {
	ProviderSubID      string
	ProviderCustomerID string
	// ProductID is the provider's product. It is what resolves a plan slug -- from
	// config for a catalog tier, from the org's row for a negotiated deal.
	ProductID string
	// OrgID is metadata.org_id, set on every checkout pug starts. Empty falls back
	// to attribution by customer id.
	OrgID string

	Status SubStatus
	// ProviderStatus is the provider's own word, kept verbatim so support can see
	// what was actually said when pug's mapping is the thing in question.
	ProviderStatus string

	PriceCents int64
	Currency   string

	CurrentPeriodStart time.Time
	CurrentPeriodEnd   time.Time
}

// IsZero reports the "nothing to apply" disposition.
func (e SubscriptionEvent) IsZero() bool { return e.ProviderSubID == "" }

type CheckoutInput struct {
	ProductID string
	OrgID     string
	// ReturnURL is where the provider sends the buyer after checkout. It is the
	// dashboard's billing page, not a provider page.
	ReturnURL string
	// CustomerEmail pre-fills the provider's form. Never the identity: a person can
	// admin two orgs, and a customer shared between them would let attribution --
	// which falls back to the customer id -- land a delivery on the wrong tenant.
	CustomerEmail string
}

// SubStatus is pug's subscription vocabulary. It is fixed and does not grow to
// fit a provider.
//
// A STORED value may nonetheless sit outside it: a provider state pug has no
// word for is stored verbatim, and ParseSubStatus then refuses it, which makes
// it not live. That is the safe direction -- an unknown state can only ever
// withhold a plan, never grant one -- and it is what lets a provider mapping be
// incomplete on its first day without leaving a lapsing org on its last known
// status.
type SubStatus string

const (
	SubStatusActive    SubStatus = "active"
	SubStatusPastDue   SubStatus = "past_due"
	SubStatusPaused    SubStatus = "paused"
	SubStatusCancelled SubStatus = "cancelled"
	SubStatusExpired   SubStatus = "expired"
	SubStatusFailed    SubStatus = "failed"
)

// Live reports whether the subscription supplies a plan. past_due is live on
// purpose: the card failed, the entitlement does not. Degrading a paying
// customer's product over an expired card is worse for both sides than a few
// unbilled days, and the delivery that normalizes to cancelled is what finally
// drops the org to the floor.
// The same set is hardcoded in three SQL sites -- GetLiveBillingSubscription,
// ListPaidEntitlementsWithoutLiveSubscription's join, and the
// billing_subscriptions_one_live_idx predicate. Nothing links them to this
// switch, so TestTheLiveStatusSetAgreesBetweenGoAndSQL walks the whole
// vocabulary through the real query; adding a member here means editing all four.
func (s SubStatus) Live() bool {
	switch s {
	case SubStatusActive, SubStatusPastDue:
		return true
	case SubStatusPaused, SubStatusCancelled, SubStatusExpired, SubStatusFailed:
		return false
	}
	// A provider word pug has no name for.
	return false
}

// AllSubStatuses is pug's whole vocabulary, so a table-driven mapping can assert
// it covers them. The column itself is not constrained to these -- see SubStatus.
func AllSubStatuses() []SubStatus {
	return []SubStatus{
		SubStatusActive, SubStatusPastDue, SubStatusPaused,
		SubStatusCancelled, SubStatusExpired, SubStatusFailed,
	}
}

// ParseSubStatus narrows a stored word back to the vocabulary. An unrecognized
// value is not live, which is the safe direction -- see SubStatus.
func ParseSubStatus(v string) (SubStatus, bool) {
	switch s := SubStatus(v); s {
	case SubStatusActive, SubStatusPastDue, SubStatusPaused,
		SubStatusCancelled, SubStatusExpired, SubStatusFailed:
		return s, true
	}
	return "", false
}

// Subscription is the stored mirror row, as resolution consumes it.
type Subscription struct {
	PlanSlug   string
	Status     SubStatus
	PriceCents int64
	Currency   string

	ProviderCustomerID string
	ProviderSubID      string

	CurrentPeriodStart time.Time
	CurrentPeriodEnd   time.Time
}
