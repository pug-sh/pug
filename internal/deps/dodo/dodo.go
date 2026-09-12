// Package dodo is the only package that imports the Dodo Payments SDK. Nothing
// above billing.PaymentProvider knows its payload shapes or signature scheme.
package dodo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	dodopayments "github.com/dodopayments/dodopayments-go"
	"github.com/dodopayments/dodopayments-go/option"
	"github.com/dodopayments/dodopayments-go/shared"
	corebilling "github.com/pug-sh/pug/internal/core/billing"
)

// Name is the provider slug: stored on every row this package produces and the
// path segment the webhook mounts at, so changing it orphans rows and 404s.
const Name = "dodo"

const (
	EnvironmentTest = "test"
	EnvironmentLive = "live"
)

// One stalled connection must not eat a dashboard request's budget, nor a cron
// pass's 30m. The SDK applies it per attempt inside its own retry loop, so a hung
// read costs this much times MaxRetries+1 -- except Charge, which retries none.
const requestTimeout = 10 * time.Second

// Config is the provider's own credentials, under its own prefix rather than a
// generic PUG_PAYMENTS_*, so a second provider's keys sit beside these.
type Config struct {
	APIKey        string `env:"PUG_DODO_API_KEY"`
	Environment   string `env:"PUG_DODO_ENVIRONMENT,default=test"`
	WebhookSecret string `env:"PUG_DODO_WEBHOOK_SECRET"`
	// MandateProduct is the one on-demand subscription product every org
	// authorizes a card against. Its stored price is never charged.
	MandateProduct string `env:"PUG_DODO_MANDATE_PRODUCT"`
}

type Client struct {
	api      *dodopayments.Client
	verifier *verifier
}

var _ corebilling.PaymentProvider = (*Client)(nil)

// New returns nil when no API key is configured: billing with no provider
// credentials is a supported mode, where only the buy button is missing.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, nil
	}

	opts := []option.RequestOption{
		option.WithBearerToken(cfg.APIKey),
		option.WithRequestTimeout(requestTimeout),
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Environment)) {
	case EnvironmentLive:
		opts = append(opts, option.WithEnvironmentLiveMode())
	case EnvironmentTest, "":
		opts = append(opts, option.WithEnvironmentTestMode())
	default:
		// Fails startup rather than defaulting: a typo'd value silently pointing live
		// traffic at the test environment takes real money nowhere.
		return nil, fmt.Errorf("dodo: unknown PUG_DODO_ENVIRONMENT %q (want %s or %s)",
			cfg.Environment, EnvironmentTest, EnvironmentLive)
	}

	c := &Client{api: dodopayments.NewClient(opts...)}
	if secret := strings.TrimSpace(cfg.WebhookSecret); secret != "" {
		v, err := newVerifier(secret)
		if err != nil {
			return nil, err
		}
		c.verifier = v
	}
	return c, nil
}

func (c *Client) Name() string { return Name }

func (c *Client) CanVerify() bool { return c != nil && c.verifier != nil }

// CreateCheckoutSession opens a mandate-only on-demand checkout: the card is
// authorized and nothing is charged.
func (c *Client) CreateCheckoutSession(
	ctx context.Context, in corebilling.CheckoutInput,
) (sessionID, checkoutURL string, err error) {
	req := dodopayments.CheckoutSessionRequestParam{
		ProductCart: dodopayments.F([]dodopayments.ProductItemReqParam{{
			ProductID: dodopayments.F(in.ProductID),
			Quantity:  dodopayments.F(int64(1)),
		}}),
		SubscriptionData: dodopayments.F(dodopayments.SubscriptionDataParam{
			OnDemand: dodopayments.F(dodopayments.OnDemandSubscriptionParam{
				MandateOnly: dodopayments.F(true),
			}),
		}),
	}
	// The ref is what proves the org; org_id is a convenience for the dashboard and
	// every reader falls back rather than depending on it propagating.
	md := dodopayments.MetadataParam{metadataOrgID: shared.UnionString(in.OrgID)}
	if in.CheckoutRef != "" {
		md[metadataCheckoutRef] = shared.UnionString(in.CheckoutRef)
	}
	req.Metadata = dodopayments.F(md)
	custom := customization(in.Theme)
	custom.ShowOnDemandTag = dodopayments.F(true)
	req.Customization = dodopayments.F(custom)
	if in.ReturnURL != "" {
		req.ReturnURL = dodopayments.F(in.ReturnURL)
	}
	// USD needs both: selection is on by default, and billing_currency is ignored while
	// adaptive pricing is off — alone the flag would just lock in the detected locale.
	req.BillingCurrency = dodopayments.F(dodopayments.CurrencyUsd)
	req.FeatureFlags = dodopayments.F(dodopayments.CheckoutSessionFlagsParam{
		AllowCurrencySelection:     dodopayments.F(false),
		AllowDiscountCode:          dodopayments.F(false),
		AllowPhoneNumberCollection: dodopayments.F(false),
		AllowCustomerEditingEmail:  dodopayments.F(true),
		AllowCustomerEditingName:   dodopayments.F(true),
		// A new customer per checkout, never a lookup by email: one person can admin two
		// orgs, and a shared customer would misattribute a delivery.
		AlwaysCreateNewCustomer: dodopayments.F(true),
	})
	if in.CustomerEmail != "" {
		customer := dodopayments.NewCustomerParam{Email: dodopayments.F(in.CustomerEmail)}
		if in.CustomerName != "" {
			customer.Name = dodopayments.F(in.CustomerName)
		}
		req.Customer = dodopayments.F[dodopayments.CustomerRequestUnionParam](customer)
	}

	session, err := c.api.CheckoutSessions.New(ctx, dodopayments.CheckoutSessionNewParams{
		CheckoutSessionRequest: req,
	})
	if err != nil {
		return "", "", fmt.Errorf("dodo: create checkout session: %w", err)
	}
	if session.CheckoutURL == "" {
		return "", "", errors.New("dodo: checkout session has no checkout_url")
	}
	return session.SessionID, session.CheckoutURL, nil
}

func (c *Client) CreatePortalSession(ctx context.Context, customerID string) (string, error) {
	session, err := c.api.Customers.CustomerPortal.New(ctx, customerID,
		dodopayments.CustomerCustomerPortalNewParams{})
	if err != nil {
		return "", fmt.Errorf("dodo: create portal session: %w", err)
	}
	if session.Link == "" {
		return "", errors.New("dodo: portal session has no link")
	}
	return session.Link, nil
}

// FetchSubscription re-reads one subscription in the same shape a delivery
// carries, so the reconcile pass and the webhook share an apply path.
func (c *Client) FetchSubscription(ctx context.Context, providerSubID string) (corebilling.SubscriptionEvent, error) {
	sub, err := c.api.Subscriptions.Get(ctx, providerSubID)
	if err != nil {
		if notFound(err) {
			return corebilling.SubscriptionEvent{}, fmt.Errorf("%w: %s",
				corebilling.ErrSubscriptionNotFound, providerSubID)
		}
		return corebilling.SubscriptionEvent{}, fmt.Errorf("dodo: get subscription: %w", err)
	}
	return c.eventFromSubscription(subscriptionPayload{
		CancelAtNextBillingDate: sub.CancelAtNextBillingDate,
		CancelledAt:             optionalTime(sub.CancelledAt),
		Currency:                string(sub.Currency),
		CustomerID:              sub.Customer.CustomerID,
		ExpiresAt:               optionalTime(sub.ExpiresAt),
		Metadata:                stringMetadata(sub.Metadata),
		NextBillingDate:         &sub.NextBillingDate,
		OnDemand:                sub.OnDemand,
		PreviousBillingDate:     &sub.PreviousBillingDate,
		ProductID:               sub.ProductID,
		RecurringPreTaxAmount:   sub.RecurringPreTaxAmount,
		Status:                  string(sub.Status),
		SubscriptionID:          sub.SubscriptionID,
	}), nil
}

// FetchCheckoutOutcome walks one checkout to the subscription it produced:
// session -> payment -> subscription. A mandate-only session's authorization may
// name no subscription, so the customer's subscriptions are then listed and the
// one carrying the checkout's ref is taken.
func (c *Client) FetchCheckoutOutcome(ctx context.Context, sessionID string) (corebilling.SubscriptionEvent, error) {
	session, err := c.api.CheckoutSessions.Get(ctx, sessionID)
	if err != nil {
		if notFound(err) {
			return corebilling.SubscriptionEvent{}, corebilling.ErrCheckoutFailed
		}
		return corebilling.SubscriptionEvent{}, fmt.Errorf("dodo: get checkout session: %w", err)
	}
	if terminalIntent(string(session.PaymentStatus)) {
		return corebilling.SubscriptionEvent{}, corebilling.ErrCheckoutFailed
	}
	if session.PaymentID == "" {
		return corebilling.SubscriptionEvent{}, nil
	}
	payment, err := c.api.Payments.Get(ctx, session.PaymentID)
	if err != nil {
		if notFound(err) {
			return corebilling.SubscriptionEvent{}, corebilling.ErrCheckoutFailed
		}
		return corebilling.SubscriptionEvent{}, fmt.Errorf("dodo: get checkout payment: %w", err)
	}
	if terminalIntent(string(payment.Status)) {
		return corebilling.SubscriptionEvent{}, corebilling.ErrCheckoutFailed
	}
	md := stringMetadata(payment.Metadata)
	subID := payment.SubscriptionID
	if subID == "" {
		if subID, err = c.findSubscription(ctx, payment.Customer.CustomerID, md[metadataCheckoutRef], session.CreatedAt); err != nil {
			return corebilling.SubscriptionEvent{}, err
		}
		if subID == "" {
			return corebilling.SubscriptionEvent{}, nil
		}
	}
	event, err := c.FetchSubscription(ctx, subID)
	if err != nil {
		return corebilling.SubscriptionEvent{}, err
	}
	// A zero here is a shape change, not "not yet": the id was just resolved.
	// Passed through, it would leave the buyer polling for good.
	if event.IsZero() {
		return corebilling.SubscriptionEvent{}, fmt.Errorf("dodo: subscription %s decoded to nothing", subID)
	}
	// Dodo calls a checkout's metadata the PAYMENT's and pug reads both off the
	// SUBSCRIPTION; fall back rather than depend on whether it propagates.
	if event.OrgID == "" {
		event.OrgID = md[metadataOrgID]
	}
	if event.CheckoutRef == "" {
		event.CheckoutRef = md[metadataCheckoutRef]
	}
	return event, nil
}

// findSubscription is the second route to a mandate: the customer's
// subscriptions since the checkout opened, matched on the ref pug minted. Every
// checkout creates its own customer, so a match is the only candidate anyway.
func (c *Client) findSubscription(ctx context.Context, customerID, ref string, since time.Time) (string, error) {
	if customerID == "" || ref == "" {
		return "", nil
	}
	iter := c.api.Subscriptions.ListAutoPaging(ctx, dodopayments.SubscriptionListParams{
		CustomerID:   dodopayments.F(customerID),
		CreatedAtGte: dodopayments.F(since.UTC().Add(-time.Minute)),
		PageNumber:   dodopayments.F(int64(0)),
		PageSize:     dodopayments.F(int64(100)),
	})
	for iter.Next() {
		sub := iter.Current()
		if stringMetadata(sub.Metadata)[metadataCheckoutRef] == ref {
			return sub.SubscriptionID, nil
		}
	}
	if err := iter.Err(); err != nil {
		return "", fmt.Errorf("dodo: list subscriptions: %w", err)
	}
	return "", nil
}

// Charge takes the amount pug computed against the mandate. Dodo's endpoint has
// no idempotency key, so anything but a definitive refusal is ambiguous and the
// caller settles it by reading. Retries are off for this one call: the SDK would
// re-POST on a reset or a 5xx and take the money twice.
func (c *Client) Charge(ctx context.Context, in corebilling.ChargeInput) (string, error) {
	res, err := c.api.Subscriptions.Charge(ctx, in.ProviderSubID, dodopayments.SubscriptionChargeParams{
		ProductPrice:       dodopayments.F(in.AmountCents),
		ProductCurrency:    dodopayments.F(dodopayments.Currency(strings.ToUpper(in.Currency))),
		ProductDescription: dodopayments.F(in.Description),
		Metadata: dodopayments.F(dodopayments.MetadataParam{
			metadataOrgID:       shared.UnionString(in.OrgID),
			metadataInvoiceID:   shared.UnionString(in.InvoiceID),
			metadataPeriodStart: shared.UnionString(in.PeriodStart.UTC().Format(time.RFC3339)),
		}),
	}, option.WithMaxRetries(0))
	if err != nil {
		var apiErr *dodopayments.Error
		if errors.As(err, &apiErr) && apiErr.StatusCode >= 400 && apiErr.StatusCode < 500 {
			if apiErr.StatusCode == http.StatusNotFound {
				return "", fmt.Errorf("%w: %s", corebilling.ErrMandateNotChargeable, in.ProviderSubID)
			}
			if declined(apiErr.StatusCode) {
				return "", &corebilling.DeclineError{
					Code:    fmt.Sprintf("HTTP_%d", apiErr.StatusCode),
					Message: apiErr.Error(),
				}
			}
		}
		return "", fmt.Errorf("dodo: charge subscription: %w", err)
	}
	if res.PaymentID == "" {
		return "", errors.New("dodo: charge returned no payment_id")
	}
	return res.PaymentID, nil
}

// ListPayments lists the payments made against a mandate since an instant.
func (c *Client) ListPayments(ctx context.Context, providerSubID string, since time.Time) ([]corebilling.PaymentRecord, error) {
	iter := c.api.Payments.ListAutoPaging(ctx, dodopayments.PaymentListParams{
		SubscriptionID: dodopayments.F(providerSubID),
		CreatedAtGte:   dodopayments.F(since.UTC()),
		PageNumber:     dodopayments.F(int64(0)),
		PageSize:       dodopayments.F(int64(100)),
	})
	var out []corebilling.PaymentRecord
	for iter.Next() {
		p := iter.Current()
		// The list endpoint carries no decline reason, and no tax line to take
		// total_amount back to the pre-tax figure pug billed -- so no amount either,
		// rather than one the caller would compare against the wrong base. settle
		// fetches the payment when it needs either.
		out = append(out, corebilling.PaymentRecord{
			PaymentID:  p.PaymentID,
			InvoiceID:  stringMetadata(p.Metadata)[metadataInvoiceID],
			Status:     paymentStatus(string(p.Status)),
			InvoiceURL: p.InvoiceURL,
			Currency:   strings.ToUpper(string(p.Currency)),
			CreatedAt:  p.CreatedAt,
		})
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("dodo: list payments: %w", err)
	}
	return out, nil
}

func (c *Client) FetchPayment(ctx context.Context, paymentID string) (corebilling.PaymentRecord, error) {
	p, err := c.api.Payments.Get(ctx, paymentID)
	if err != nil {
		if notFound(err) {
			return corebilling.PaymentRecord{}, fmt.Errorf("%w: %s", corebilling.ErrPaymentNotFound, paymentID)
		}
		return corebilling.PaymentRecord{}, fmt.Errorf("dodo: get payment: %w", err)
	}
	return corebilling.PaymentRecord{
		PaymentID:    p.PaymentID,
		InvoiceID:    stringMetadata(p.Metadata)[metadataInvoiceID],
		Status:       paymentStatus(string(p.Status)),
		ErrorCode:    p.ErrorCode,
		ErrorMessage: p.ErrorMessage,
		InvoiceURL:   p.InvoiceURL,
		AmountCents:  p.TotalAmount - p.Tax,
		Currency:     strings.ToUpper(string(p.Currency)),
		CreatedAt:    p.CreatedAt,
	}, nil
}

// SetNextBillingDate pins when a portal cancellation takes effect: just after
// pug's next charge rather than before it.
func (c *Client) SetNextBillingDate(ctx context.Context, providerSubID string, at time.Time) error {
	_, err := c.api.Subscriptions.Update(ctx, providerSubID, dodopayments.SubscriptionUpdateParams{
		NextBillingDate: dodopayments.F(at.UTC()),
	})
	if err != nil {
		return fmt.Errorf("dodo: set next billing date: %w", err)
	}
	return nil
}

func (c *Client) CancelSubscription(ctx context.Context, providerSubID string) error {
	_, err := c.api.Subscriptions.Update(ctx, providerSubID, dodopayments.SubscriptionUpdateParams{
		Status:       dodopayments.F(dodopayments.SubscriptionStatusCancelled),
		CancelReason: dodopayments.F(dodopayments.SubscriptionUpdateParamsCancelReasonCancelledByCustomer),
	})
	if err != nil {
		return fmt.Errorf("dodo: cancel subscription: %w", err)
	}
	return nil
}

// terminalIntent reports a payment Dodo will not carry further. A short allowlist
// on purpose: anything else stays "not yet", so it can delay but never invent one.
func terminalIntent(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "failed", "cancelled", "canceled":
		return true
	}
	return false
}

// paymentStatus narrows Dodo's intent statuses to settled, failed, or not yet.
func paymentStatus(status string) corebilling.PaymentStatus {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "succeeded":
		return corebilling.PaymentSucceeded
	case "failed", "cancelled", "canceled":
		return corebilling.PaymentFailed
	}
	return corebilling.PaymentPending
}

// stringMetadata narrows Dodo's string|number|bool metadata to the string values
// pug writes. A non-string value was not written by us, so it is dropped.
func stringMetadata(in dodopayments.Metadata) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if s, ok := v.(shared.UnionString); ok {
			out[k] = string(s)
		}
	}
	return out
}

func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// declined is a 4xx that means the card or the mandate refused, and only that.
// Every other 4xx -- a bad key, a rate limit, a route or content type Dodo did
// not accept -- is pug's problem, and an allow-list is what keeps a dependency
// bump from dunning every customer at once. The rest go to the ambiguous branch,
// which is settled by reading and fails the pass.
func declined(status int) bool {
	return status == http.StatusPaymentRequired
}

// notFound separates "the provider does not have this" from "could not be
// reached": a finding versus an outage, identical errors without the status code.
func notFound(err error) bool {
	var apiErr *dodopayments.Error
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}
