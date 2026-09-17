// Package dodo is the only package that imports the Dodo Payments SDK. Nothing
// above billing.PaymentProvider knows its payload shapes or signature scheme.
package dodo

import (
	"context"
	"encoding/json"
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

// One stalled connection must not eat a dashboard request's budget, nor the
// reconcile pass's 30m — which one hung read could otherwise spend entirely.
const requestTimeout = 10 * time.Second

// Config is the provider's own credentials, under its own prefix rather than a
// generic PUG_PAYMENTS_*, so a second provider's keys sit beside these.
type Config struct {
	APIKey      string `env:"PUG_DODO_API_KEY"`
	Environment string `env:"PUG_DODO_ENVIRONMENT,default=test"`
	// MandateProduct is the one product every org authorizes against. Its stored
	// price is never charged: pug prices the period and charges that amount.
	// Absent means nothing is purchasable, as a missing tier key used to.
	MandateProduct string `env:"PUG_DODO_MANDATE_PRODUCT"`
	WebhookSecret  string `env:"PUG_DODO_WEBHOOK_SECRET"`
}

type Client struct {
	api      *dodopayments.Client
	verifier *verifier
}

var _ corebilling.PaymentProvider = (*Client)(nil)

// New returns nil when no API key is configured: billing with no provider
// credentials is a supported mode, where only the buy button is missing. The
// webhook secret is needed on top of a key — no key means no client, so no route.
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

// A webhook secret is what decides the route mounts at all.
func (c *Client) CanVerify() bool { return c != nil && c.verifier != nil }

func (c *Client) CreateCheckoutSession(
	ctx context.Context, in corebilling.CheckoutInput,
) (sessionID, checkoutURL string, err error) {
	req := dodopayments.CheckoutSessionRequestParam{
		ProductCart: dodopayments.F([]dodopayments.ProductItemReqParam{{
			ProductID: dodopayments.F(in.ProductID),
			Quantity:  dodopayments.F(int64(1)),
		}}),
		// The whole point of the checkout: authorize the card and charge nothing. Pug
		// prices each period and charges that against the mandate it leaves behind.
		SubscriptionData: dodopayments.F(dodopayments.SubscriptionDataParam{
			OnDemand: dodopayments.F(dodopayments.OnDemandSubscriptionParam{
				MandateOnly: dodopayments.F(true),
			}),
		}),
	}
	// Both ride every delivery this checkout produces; only the ref proves the org.
	md := dodopayments.MetadataParam{metadataOrgID: shared.UnionString(in.OrgID)}
	if in.CheckoutRef != "" {
		md[metadataCheckoutRef] = shared.UnionString(in.CheckoutRef)
	}
	req.Metadata = dodopayments.F(md)
	custom := customization(in.Theme)
	// So the page says what it is: a card being authorized, not charged.
	custom.ShowOnDemandTag = dodopayments.F(true)
	req.Customization = dodopayments.F(custom)
	if in.ReturnURL != "" {
		req.ReturnURL = dodopayments.F(in.ReturnURL)
	}
	// USD needs both: selection is on by default, and billing_currency is ignored while
	// adaptive pricing is off — alone the flag would just lock in the detected locale.
	req.BillingCurrency = dodopayments.F(dodopayments.CurrencyUsd)
	req.FeatureFlags = dodopayments.F(dodopayments.CheckoutSessionFlagsParam{
		AllowCurrencySelection: dodopayments.F(false),
		// Pug mints no codes, so the box only invites a buyer to go hunting for one.
		AllowDiscountCode: dodopayments.F(false),
		// Defaults on, and a phone number buys an analytics upgrade nothing.
		AllowPhoneNumberCollection: dodopayments.F(false),
		// Dodo's default, sent explicitly because pug's prices exclude tax: a business
		// giving its VAT or GST number is how the tax added on top comes out right.
		AllowTaxID: dodopayments.F(true),
		// Pre-filled from the account and otherwise frozen for the session — but the name
		// can be a stale OIDC claim and the receipt often wants accounts payable, and
		// attribution rides the metadata ref rather than either of these.
		AllowCustomerEditingEmail: dodopayments.F(true),
		AllowCustomerEditingName:  dodopayments.F(true),
		// A new customer per checkout, never a lookup by email: one person can admin two
		// orgs, and a shared customer would misattribute a delivery.
		AlwaysCreateNewCustomer: dodopayments.F(true),
	})
	if in.CustomerEmail != "" {
		customer := dodopayments.NewCustomerParam{Email: dodopayments.F(in.CustomerEmail)}
		// Sent only when there is one: a name given here is frozen for the session, and
		// an empty one leaves the buyer to type theirs rather than showing a blank field.
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
	return c.eventFromSDK(sub), nil
}

// SetNextBillingDate is only a PATCH, so the SDK's retry is safe here.
func (c *Client) SetNextBillingDate(
	ctx context.Context, providerSubID string, at time.Time,
) (corebilling.SubscriptionEvent, error) {
	sub, err := c.api.Subscriptions.Update(ctx, providerSubID, dodopayments.SubscriptionUpdateParams{
		NextBillingDate: dodopayments.F(at.UTC()),
	})
	if err != nil {
		return corebilling.SubscriptionEvent{}, fmt.Errorf("dodo: set next billing date: %w", err)
	}
	return c.eventFromSDK(sub), nil
}

func (c *Client) CancelSubscription(ctx context.Context, providerSubID string) (corebilling.SubscriptionEvent, error) {
	sub, err := c.api.Subscriptions.Update(ctx, providerSubID, dodopayments.SubscriptionUpdateParams{
		CancelReason: dodopayments.F(dodopayments.SubscriptionUpdateParamsCancelReasonCancelledByCustomer),
		Status:       dodopayments.F(dodopayments.SubscriptionStatusCancelled),
	})
	if err != nil {
		return corebilling.SubscriptionEvent{}, fmt.Errorf("dodo: cancel subscription: %w", err)
	}
	return c.eventFromSDK(sub), nil
}

func (c *Client) eventFromSDK(sub *dodopayments.Subscription) corebilling.SubscriptionEvent {
	return c.eventFromSubscription(subscriptionPayload{
		CancelAtNextBillingDate: sub.CancelAtNextBillingDate,
		CancelledAt:             optionalTime(sub.CancelledAt),
		Currency:                string(sub.Currency),
		CustomerID:              sub.Customer.CustomerID,
		ExpiresAt:               optionalTime(sub.ExpiresAt),
		Metadata:                stringMetadata(sub.Metadata),
		NextBillingDate:         &sub.NextBillingDate,
		OnDemand:                &sub.OnDemand,
		PreviousBillingDate:     &sub.PreviousBillingDate,
		ProductID:               sub.ProductID,
		Status:                  string(sub.Status),
		SubscriptionID:          sub.SubscriptionID,
		TaxInclusive:            &sub.TaxInclusive,
	})
}

// FetchCheckoutOutcome walks one checkout to the subscription it produced:
// session -> payment -> subscription, the last hop through FetchSubscription so the
// event matches a delivery's. A dead payment is read as dead, not as "not yet".
func (c *Client) FetchCheckoutOutcome(ctx context.Context, sessionID string) (corebilling.SubscriptionEvent, error) {
	session, err := c.api.CheckoutSessions.Get(ctx, sessionID)
	if err != nil {
		// A session Dodo has never heard of, or has aged out, it will never settle.
		// Reported as a dead checkout so the buyer is told rather than polling a 500.
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
		// A mandate-only authorization may name no subscription. The session carries no
		// customer of its own, so the payment's is the only route left to one.
		if subID, err = c.findSubscription(ctx,
			payment.Customer.CustomerID, md[metadataCheckoutRef], session.CreatedAt); err != nil {
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
	// The id was resolved just now, so a zero here is a shape change, not "not yet".
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

// Charge POSTs once. The endpoint takes no idempotency key, so the SDK's retry would
// re-send a charge that may already have taken the money.
func (c *Client) Charge(ctx context.Context, in corebilling.ChargeInput) (string, error) {
	res, err := c.api.Subscriptions.Charge(ctx, in.ProviderSubID, dodopayments.SubscriptionChargeParams{
		Metadata: dodopayments.F(dodopayments.MetadataParam{
			metadataInvoiceID:   shared.UnionString(in.InvoiceID),
			metadataOrgID:       shared.UnionString(in.OrgID),
			metadataPeriodStart: shared.UnionString(in.PeriodStart.UTC().Format(time.RFC3339)),
		}),
		ProductCurrency:    dodopayments.F(dodopayments.Currency(in.Currency)),
		ProductDescription: dodopayments.F(in.Description),
		ProductPrice:       dodopayments.F(in.AmountCents),
	}, option.WithMaxRetries(0))
	if err != nil {
		var apiErr *dodopayments.Error
		if !errors.As(err, &apiErr) || apiErr.StatusCode < 400 || apiErr.StatusCode >= 500 {
			return "", fmt.Errorf("dodo: charge subscription: %w", err)
		}
		code := fmt.Sprintf("HTTP_%d", apiErr.StatusCode)
		// A decline's own code is what tells a hard one from a soft one.
		var body struct {
			Code string `json:"code"`
		}
		if apiErr.StatusCode == http.StatusPaymentRequired &&
			json.Unmarshal([]byte(apiErr.JSON.RawJSON()), &body) == nil && body.Code != "" {
			code = body.Code
		}
		return "", &corebilling.ChargeError{
			Code:    code,
			Message: apiErr.Error(),
			// Only a 402: dunning any other 4xx would dun every customer over a rotated key.
			Declined:      apiErr.StatusCode == http.StatusPaymentRequired,
			NotChargeable: apiErr.StatusCode == http.StatusNotFound,
		}
	}
	if res.PaymentID == "" {
		return "", errors.New("dodo: charge returned no payment_id")
	}
	return res.PaymentID, nil
}

// ListPayments is a mandate's payments since an instant, with no amounts: the list
// carries total_amount but not the tax inside it.
func (c *Client) ListPayments(ctx context.Context, providerSubID string, since time.Time) ([]corebilling.Payment, error) {
	iter := c.api.Payments.ListAutoPaging(ctx, dodopayments.PaymentListParams{
		CreatedAtGte: dodopayments.F(since.UTC()),
		// The auto-pager reads an absent page number as 1 and asks for page 2 next.
		PageNumber:     dodopayments.F(int64(0)),
		PageSize:       dodopayments.F(int64(100)),
		SubscriptionID: dodopayments.F(providerSubID),
	})
	var out []corebilling.Payment
	for iter.Next() {
		p := iter.Current()
		out = append(out, corebilling.Payment{
			CreatedAt:     p.CreatedAt,
			InvoiceID:     stringMetadata(p.Metadata)[metadataInvoiceID],
			PaymentID:     p.PaymentID,
			ProviderSubID: p.SubscriptionID,
			Status:        paymentStatus(string(p.Status)),
		})
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("dodo: list payments: %w", err)
	}
	return out, nil
}

func (c *Client) FetchPayment(ctx context.Context, paymentID string) (corebilling.Payment, error) {
	p, err := c.api.Payments.Get(ctx, paymentID)
	if err != nil {
		return corebilling.Payment{}, fmt.Errorf("dodo: get payment: %w", err)
	}
	return paymentFrom(paymentPayload{
		CreatedAt:      p.CreatedAt,
		Currency:       string(p.Currency),
		ErrorCode:      p.ErrorCode,
		ErrorMessage:   p.ErrorMessage,
		InvoiceURL:     p.InvoiceURL,
		Metadata:       stringMetadata(p.Metadata),
		PaymentID:      p.PaymentID,
		Status:         string(p.Status),
		SubscriptionID: p.SubscriptionID,
		Tax:            p.Tax,
		TotalAmount:    p.TotalAmount,
	}), nil
}

// findSubscription is the second route to a mandate: the customer's subscriptions
// since the checkout opened, matched on the ref pug minted — a customer can hold
// more than one.
func (c *Client) findSubscription(ctx context.Context, customerID, ref string, since time.Time) (string, error) {
	if customerID == "" || ref == "" {
		return "", nil
	}
	iter := c.api.Subscriptions.ListAutoPaging(ctx, dodopayments.SubscriptionListParams{
		CustomerID: dodopayments.F(customerID),
		// A minute back: the subscription is created from the same checkout, and the
		// two clocks are not pug's to reconcile.
		CreatedAtGte: dodopayments.F(since.UTC().Add(-time.Minute)),
		// The auto-pager reads an absent page number as 1 and asks for page 2 next.
		PageNumber: dodopayments.F(int64(0)),
		PageSize:   dodopayments.F(int64(100)),
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

func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
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

// notFound separates "the provider does not have this" from "could not be
// reached": a finding versus an outage, identical errors without the status code.
func notFound(err error) bool {
	var apiErr *dodopayments.Error
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}
