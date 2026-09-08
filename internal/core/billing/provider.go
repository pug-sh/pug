package billing

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// ErrUndecodable is a delivery that VERIFIED and still will not decode. Kept apart
// from a signature failure so it is retried and recorded, not answered 401.
var ErrUndecodable = errors.New("billing: webhook body cannot be decoded")

// PaymentProvider is the whole seam between pug and a merchant of record: nothing
// else in this slice imports a provider package. The payload is deliberately not
// abstracted -- a common payload schema across providers cannot be maintained.
type PaymentProvider interface {
	// Name is the provider's slug, stored on every row it produces and the path
	// segment its webhook mounts at -- changing it orphans stored rows.
	Name() string

	// Verify authenticates a raw delivery, taking the exact bytes because every
	// signature scheme signs those, not a decoded message.
	Verify(headers http.Header, rawBody []byte) (Delivery, error)

	// Normalize maps one verified delivery onto pug's vocabulary. A zero
	// SubscriptionEvent means "store, mark processed, ignore".
	Normalize(Delivery) (SubscriptionEvent, error)

	// CreateCheckoutSession returns the session's id alongside the buyer's URL. The
	// id is what ConfirmCheckout re-reads by; an empty one leaves only the webhook.
	CreateCheckoutSession(ctx context.Context, in CheckoutInput) (sessionID, checkoutURL string, err error)
	CreatePortalSession(ctx context.Context, customerID string) (string, error)
	// FetchSubscription re-reads one subscription for the reconcile pass, in the
	// same shape a delivery would have carried.
	FetchSubscription(ctx context.Context, providerSubID string) (SubscriptionEvent, error)
	// FetchCheckoutOutcome re-reads one checkout the dashboard started, in that same
	// shape. A zero SubscriptionEvent means "not settled yet" and is not an error; a
	// checkout the provider gave up on must return ErrCheckoutFailed instead.
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
	// DeliveredAt bounds payload freshness and is the CAS guard. From the signed
	// envelope, the one stamp uniform across event types.
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
	// OrgID is metadata.org_id, which a buyer can set on a static payment link. It
	// attributes only beside a ProductID an operator staged, and cross-checks a confirm.
	OrgID string
	// CheckoutRef is the token pug minted and stored before opening the checkout --
	// the one signal a buyer cannot forge, so it wins.
	CheckoutRef string

	Status SubStatus
	// ProviderStatus is the provider's own word, kept verbatim for when pug's
	// mapping is the thing in question.
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
	// CheckoutRef is the token pug stored against OrgID before calling the provider.
	// It rides the metadata and comes back on every delivery.
	CheckoutRef string
	// ReturnURL is where the provider sends the buyer after checkout. It is the
	// dashboard's billing page, not a provider page.
	ReturnURL string
	// CustomerEmail pre-fills the provider's form. Never the identity: a person can
	// admin two orgs, and a shared customer would misattribute a delivery.
	CustomerEmail string
	// CustomerName pre-fills the same form and is often empty -- a magic-link signup
	// stores no name. Only an OIDC claim supplies one.
	CustomerName string
	// Theme is the palette the checkout renders in, so an overlay opened from the
	// dashboard does not land light on a dark page.
	Theme CheckoutTheme
}

// CheckoutTheme is a resolved mode, never a stored preference: "system" is already
// light or dark by the time a client sends it. Auto leaves it to the provider.
type CheckoutTheme string

const (
	CheckoutThemeAuto  CheckoutTheme = ""
	CheckoutThemeLight CheckoutTheme = "light"
	CheckoutThemeDark  CheckoutTheme = "dark"
)

// SubStatus is pug's subscription vocabulary. It is fixed and does not grow to
// fit a provider: a state pug has no word for is stored verbatim and refused by
// ParseSubStatus, which can only withhold a plan, never grant one.
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
// purpose: the card failed, the entitlement did not. The same set is hardcoded in
// four SQL sites, which TestTheLiveStatusSetAgreesBetweenGoAndSQL pins.
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
// it covers them. The column itself is not constrained to these.
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
