package billing

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// The errors an adapter must return; every other provider fault is its own.
var (
	// ErrUndecodable is a delivery that VERIFIED and still will not decode. Kept apart
	// from a signature failure so it is retried and recorded, not answered 401.
	ErrUndecodable = errors.New("billing: webhook body cannot be decoded")
	// ErrCheckoutFailed is a checkout the provider says will not settle. Distinct
	// from a zero event ("not yet"), or a decline reads as a slow payment forever.
	ErrCheckoutFailed = errors.New("billing: this checkout did not complete")
	// ErrSubscriptionNotFound is a subscription the provider no longer knows: a
	// finding for the reconcile pass, not a read failure worth retrying every run.
	ErrSubscriptionNotFound = errors.New("billing: the provider does not know this subscription")
)

// PaymentProvider is the whole seam between pug and a merchant of record: nothing
// else in this slice imports a provider package. The payload is deliberately not
// abstracted — a common payload schema across providers cannot be maintained.
type PaymentProvider interface {
	// Name is the provider's slug, stored on every row it produces and the path
	// segment its webhook mounts at — changing it orphans stored rows.
	Name() string

	// Verify authenticates a raw delivery, taking the exact bytes because every
	// signature scheme signs those, not a decoded message.
	Verify(headers http.Header, rawBody []byte) (Delivery, error)

	// CanVerify reports whether a signing secret is configured, i.e. whether Verify
	// can authenticate anything. False mounts no webhook route at all rather than
	// taking unverified deliveries on the money path.
	CanVerify() bool

	// Normalize maps one verified delivery onto pug's vocabulary. A zero
	// SubscriptionEvent passes the delivery on to NormalizePayment.
	Normalize(Delivery) (SubscriptionEvent, error)
	// NormalizePayment maps a payment or refund delivery the same way. Both zero means
	// "store, mark processed, ignore".
	NormalizePayment(Delivery) (PaymentEvent, error)

	// CreateCheckoutSession opens a mandate-only checkout: it authorizes a payment
	// method against ProductID and charges nothing. The id is what ConfirmCheckout
	// re-reads by; an empty one leaves only the webhook.
	CreateCheckoutSession(ctx context.Context, in CheckoutInput) (sessionID, checkoutURL string, err error)
	CreatePortalSession(ctx context.Context, customerID string) (string, error)
	// FetchSubscription re-reads one subscription for the reconcile pass, in the
	// same shape a delivery would have carried.
	FetchSubscription(ctx context.Context, providerSubID string) (SubscriptionEvent, error)
	// FetchCheckoutOutcome re-reads one checkout the dashboard started, in that same
	// shape. A zero SubscriptionEvent means "not settled yet" and is not an error; a
	// checkout the provider gave up on must return ErrCheckoutFailed instead.
	FetchCheckoutOutcome(ctx context.Context, sessionID string) (SubscriptionEvent, error)
	// Charge takes an amount pug computed against a mandate and returns the payment id.
	// A *ChargeError took nothing; any other error leaves the outcome unknown.
	Charge(ctx context.Context, in ChargeInput) (paymentID string, err error)
	// ListPayments is a mandate's payments created at or after since. A listed payment
	// need not carry amounts; FetchPayment reads one whole.
	ListPayments(ctx context.Context, providerSubID string, since time.Time) ([]Payment, error)
	FetchPayment(ctx context.Context, paymentID string) (Payment, error)
	// SetNextBillingDate moves where a cancellation from the provider's portal lands. Both
	// writes return the subscription as it now stands, in a delivery's shape.
	SetNextBillingDate(ctx context.Context, providerSubID string, at time.Time) (SubscriptionEvent, error)
	CancelSubscription(ctx context.Context, providerSubID string) (SubscriptionEvent, error)
}

// PaymentStatus is what a payment says about the invoice it was made for.
type PaymentStatus string

const (
	// PaymentProcessing is every state that is not yet an outcome, a word pug has no
	// name for included, so it can delay a settle but never invent one.
	PaymentProcessing PaymentStatus = "processing"
	PaymentSucceeded  PaymentStatus = "succeeded"
	PaymentFailed     PaymentStatus = "failed"
)

// Payment is one payment as the provider reports it.
type Payment struct {
	PaymentID     string
	ProviderSubID string
	// InvoiceID is the metadata every charge pug makes carries. A payment link lets a
	// buyer set it too, so it names an invoice and proves nothing.
	InvoiceID string
	Status    PaymentStatus
	// TotalCents includes TaxCents, the tax the provider added on top of pug's amount.
	TotalCents   int64
	TaxCents     int64
	Currency     string
	ErrorCode    string
	ErrorMessage string
	InvoiceURL   string
	CreatedAt    time.Time
}

// PaymentEvent is a payment or refund delivery in pug's vocabulary. The zero value
// means there is nothing to apply.
type PaymentEvent struct {
	// Payment is the payment delivered, or the one a refund returned money on.
	Payment Payment
	// RefundID is set on a refund.
	RefundID      string
	RefundCents   int64
	PartialRefund bool
}

// IsZero reports the "nothing to apply" disposition.
func (e PaymentEvent) IsZero() bool { return e.Payment.PaymentID == "" }

// ChargeInput is one charge against a mandate, in whole cents.
type ChargeInput struct {
	ProviderSubID string
	AmountCents   int64
	Currency      string
	Description   string
	InvoiceID     string
	OrgID         string
	PeriodStart   time.Time
}

// ChargeError is a charge the provider answered with a refusal, so nothing was taken.
type ChargeError struct {
	// Code is the refusal as last_error_code keeps it.
	Code    string
	Message string
	// Declined is the card's answer. A refusal with neither flag is pug's own problem.
	Declined bool
	// NotChargeable claims the mandate has ended; pug acts on it only once a read agrees.
	NotChargeable bool
}

func (e *ChargeError) Error() string { return "billing: charge refused: " + e.Code + ": " + e.Message }

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
	// ProductID is the provider's product. It is what resolves a plan slug — from
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

	Currency string

	CurrentPeriodStart time.Time
	CurrentPeriodEnd   time.Time

	// OnDemand is what makes the subscription a mandate pug charges, rather than
	// one the provider bills on its own schedule as well.
	OnDemand bool
	// TaxInclusive would carve the tax out of pug's own amount instead of adding it
	// on top, and nothing downstream would notice.
	TaxInclusive bool
	// CancelAtPeriodEnd is a cancellation the customer scheduled at the provider.
	CancelAtPeriodEnd bool
	// EndedAt is when a cancelled or expired mandate stopped, when the provider says.
	EndedAt time.Time
	// PaymentMethodUpdated is a new card on the mandate, which reopens the org's
	// failed and uncollectible invoices.
	PaymentMethodUpdated bool
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
	// CustomerName pre-fills the same form and is often empty — a magic-link signup
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
// three queries and in the one-live index, pinned by
// TestTheLiveStatusSetAgreesBetweenGoAndSQL, TestPastDueCountsAsBilled and
// TestPastDueHoldsTheOneLiveSlot.
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
// value is not live, which is the safe direction — see SubStatus.
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
	PlanSlug string
	Status   SubStatus
	Currency string

	ProviderCustomerID string
	ProviderSubID      string

	CurrentPeriodStart time.Time
	CurrentPeriodEnd   time.Time

	// OnDemand is what makes the row a mandate pug can charge at all; a close bills
	// it until EndedAt.
	OnDemand          bool
	CancelAtPeriodEnd bool
	EndedAt           time.Time

	// CreateTime is when pug first stored the mandate, which a close bills from.
	CreateTime time.Time
}
