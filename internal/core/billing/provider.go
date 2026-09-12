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
	// ErrPaymentNotFound is a payment the provider no longer knows: the charge it
	// recorded is gone, not unreadable.
	ErrPaymentNotFound = errors.New("billing: the provider does not know this payment")
	// ErrMandateNotChargeable is a charge against a mandate the provider will no
	// longer charge: cancelled, expired or otherwise ended. Never retried.
	ErrMandateNotChargeable = errors.New("billing: the mandate can no longer be charged")
)

// DeclineError is a charge the provider refused outright. Code is synthesised
// from the HTTP status, not the provider's own error_code, which reaches pug
// later on the payment record. Anything else a charge returns is ambiguous and
// settled by reading.
type DeclineError struct {
	Code    string
	Message string
}

func (e *DeclineError) Error() string { return "billing: charge declined: " + e.Code }

// PaymentProvider is the whole seam between pug and a merchant of record: nothing
// else in this package imports a provider package.
type PaymentProvider interface {
	// Name is the provider's slug, stored on every row it produces and the path
	// segment its webhook mounts at — changing it orphans stored rows.
	Name() string

	// Verify authenticates a raw delivery over the exact bytes.
	Verify(headers http.Header, rawBody []byte) (Delivery, error)

	// CanVerify reports whether a signing secret is configured. False mounts no
	// webhook route at all rather than taking unverified deliveries on the money path.
	CanVerify() bool

	// Normalize maps one verified subscription delivery onto pug's vocabulary. A
	// zero SubscriptionEvent means the delivery is not a subscription event.
	Normalize(Delivery) (SubscriptionEvent, error)
	// NormalizePayment maps a payment or refund delivery. A zero PaymentEvent means
	// "store, mark processed, ignore".
	NormalizePayment(Delivery) (PaymentEvent, error)

	// CreateCheckoutSession opens a mandate-only checkout: it authorizes a payment
	// method against ProductID and charges nothing. The id is what ConfirmCheckout
	// re-reads by; an empty one leaves only the webhook.
	CreateCheckoutSession(ctx context.Context, in CheckoutInput) (sessionID, checkoutURL string, err error)
	CreatePortalSession(ctx context.Context, customerID string) (string, error)
	FetchSubscription(ctx context.Context, providerSubID string) (SubscriptionEvent, error)
	// FetchCheckoutOutcome re-reads one checkout the dashboard started. A zero
	// SubscriptionEvent means "not settled yet"; a checkout the provider gave up on
	// must return ErrCheckoutFailed instead.
	FetchCheckoutOutcome(ctx context.Context, sessionID string) (SubscriptionEvent, error)

	// Charge takes the amount pug computed against the mandate and returns the
	// provider's payment id. A definitive refusal is a *DeclineError or
	// ErrMandateNotChargeable; any other error is ambiguous.
	Charge(ctx context.Context, in ChargeInput) (paymentID string, err error)
	// ListPayments lists the payments made against a mandate since an instant, so
	// an ambiguous charge is settled by reading rather than by charging again.
	ListPayments(ctx context.Context, providerSubID string, since time.Time) ([]PaymentRecord, error)
	FetchPayment(ctx context.Context, paymentID string) (PaymentRecord, error)
	// SetNextBillingDate pins when the provider considers the mandate's period to
	// end, which is when a portal cancellation takes effect.
	SetNextBillingDate(ctx context.Context, providerSubID string, at time.Time) error
	CancelSubscription(ctx context.Context, providerSubID string) error
}

// Delivery is one verified webhook, still in the provider's own vocabulary.
type Delivery struct {
	WebhookID  string
	EventType  string
	RawPayload []byte
	// DeliveredAt bounds payload freshness and is the CAS guard.
	DeliveredAt time.Time
}

// SubscriptionEvent is a subscription delivery in pug's vocabulary. The zero
// value means there is nothing to apply.
type SubscriptionEvent struct {
	ProviderSubID      string
	ProviderCustomerID string
	ProductID          string
	// OrgID is metadata.org_id, which a buyer can set on a static payment link. It
	// cross-checks a confirm and attributes nothing on its own.
	OrgID string
	// CheckoutRef is the token pug minted before opening the checkout: the one
	// signal a buyer cannot forge, so attribution prefers it.
	CheckoutRef string

	Status         SubStatus
	ProviderStatus string

	PriceCents int64
	Currency   string

	CurrentPeriodStart time.Time
	CurrentPeriodEnd   time.Time

	// OnDemand is what makes the subscription a mandate pug charges rather than
	// one the provider bills on its own schedule.
	OnDemand bool
	// CancelAtPeriodEnd is a cancellation the customer scheduled in the portal.
	CancelAtPeriodEnd bool
	// EndedAt is when a cancelled or expired mandate stopped, if the provider says.
	EndedAt time.Time
	// PaymentMethodUpdated is the delivery that follows a new card, which re-opens
	// the org's failed invoices.
	PaymentMethodUpdated bool
}

func (e SubscriptionEvent) IsZero() bool { return e.ProviderSubID == "" }

// PaymentStatus is a payment's outcome in pug's vocabulary; "" is not settled.
type PaymentStatus string

const (
	PaymentPending   PaymentStatus = ""
	PaymentSucceeded PaymentStatus = "succeeded"
	PaymentFailed    PaymentStatus = "failed"
)

// PaymentRecord is one payment as a delivery or a direct read carries it.
type PaymentRecord struct {
	PaymentID string
	// InvoiceID is metadata.invoice_id, which pug writes on every charge it makes.
	// Empty on a payment pug did not make, such as a mandate's authorization.
	InvoiceID string
	Status    PaymentStatus
	// ErrorCode and ErrorMessage are the provider's decline. The message is
	// merchant-facing and never crosses the wire.
	ErrorCode    string
	ErrorMessage string
	InvoiceURL   string
	// AmountCents and Currency are what the provider actually took, PRE-TAX to
	// match what pug billed -- the mandate product is tax-exclusive -- so pug can
	// check it against what it billed rather than trusting the invoice id alone.
	AmountCents int64
	Currency    string
	CreatedAt   time.Time
}

// PaymentEvent is a payment or refund delivery. For a refund, Payment.PaymentID
// is the payment refunded.
type PaymentEvent struct {
	Refund  bool
	Payment PaymentRecord
}

func (e PaymentEvent) IsZero() bool { return e.Payment.PaymentID == "" }

type CheckoutInput struct {
	// ProductID is the one mandate product every org authorizes against.
	ProductID string
	OrgID     string
	// CheckoutRef is the token pug stored against OrgID before calling the provider.
	CheckoutRef string
	ReturnURL   string
	// CustomerEmail pre-fills the provider's form. Never the identity: a person can
	// admin two orgs, and a shared customer would misattribute a delivery.
	CustomerEmail string
	CustomerName  string
	Theme         CheckoutTheme
}

type ChargeInput struct {
	ProviderSubID string
	AmountCents   int64
	Currency      string
	Description   string
	// Explicit metadata: the charge inherits the subscription's only when none is
	// passed, and every payment webhook needs the invoice id.
	InvoiceID   string
	OrgID       string
	PeriodStart time.Time
}

// CheckoutTheme is a resolved mode, never a stored preference. Auto leaves it to
// the provider.
type CheckoutTheme string

const (
	CheckoutThemeAuto  CheckoutTheme = ""
	CheckoutThemeLight CheckoutTheme = "light"
	CheckoutThemeDark  CheckoutTheme = "dark"
)

// SubStatus is pug's subscription vocabulary. A state pug has no word for is
// stored verbatim and refused by ParseSubStatus, which can only withhold.
type SubStatus string

const (
	SubStatusActive    SubStatus = "active"
	SubStatusPastDue   SubStatus = "past_due"
	SubStatusPaused    SubStatus = "paused"
	SubStatusCancelled SubStatus = "cancelled"
	SubStatusExpired   SubStatus = "expired"
	SubStatusFailed    SubStatus = "failed"
)

// Live reports whether the subscription is a mandate pug can charge. past_due is
// live on purpose: the card failed, the entitlement did not. The same set is
// hardcoded in the SQL and the one-live index.
func (s SubStatus) Live() bool {
	switch s {
	case SubStatusActive, SubStatusPastDue:
		return true
	case SubStatusPaused, SubStatusCancelled, SubStatusExpired, SubStatusFailed:
		return false
	}
	return false
}

func AllSubStatuses() []SubStatus {
	return []SubStatus{
		SubStatusActive, SubStatusPastDue, SubStatusPaused,
		SubStatusCancelled, SubStatusExpired, SubStatusFailed,
	}
}

func ParseSubStatus(v string) (SubStatus, bool) {
	switch s := SubStatus(v); s {
	case SubStatusActive, SubStatusPastDue, SubStatusPaused,
		SubStatusCancelled, SubStatusExpired, SubStatusFailed:
		return s, true
	}
	return "", false
}

// Subscription is the stored mirror row, as resolution and invoicing consume it.
type Subscription struct {
	PlanSlug string
	Status   SubStatus
	// PriceCents is the mandate product's recurring price, which pug never
	// charges: every amount is computed here and charged on demand.
	PriceCents int64
	Currency   string

	Provider           string
	ProviderCustomerID string
	ProviderSubID      string

	CurrentPeriodStart time.Time
	CurrentPeriodEnd   time.Time

	OnDemand          bool
	CancelAtPeriodEnd bool
	EndedAt           time.Time
	// CreateTime is when pug first saw the mandate: the day the card was added.
	CreateTime time.Time
	UpdateTime time.Time
}
