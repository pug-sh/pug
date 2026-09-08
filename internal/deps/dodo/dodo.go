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

// One stalled connection must not eat a dashboard request's budget, nor the
// reconcile pass's 30m — which one hung read could otherwise spend entirely.
const requestTimeout = 10 * time.Second

// Config is the provider's own credentials, under its own prefix rather than a
// generic PUG_PAYMENTS_*, so a second provider's keys sit beside these.
type Config struct {
	APIKey        string `env:"PUG_DODO_API_KEY"`
	Environment   string `env:"PUG_DODO_ENVIRONMENT,default=test"`
	WebhookSecret string `env:"PUG_DODO_WEBHOOK_SECRET"`
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
	}
	// Both ride every delivery this checkout produces; only the ref proves the org.
	md := dodopayments.MetadataParam{metadataOrgID: shared.UnionString(in.OrgID)}
	if in.CheckoutRef != "" {
		md[metadataCheckoutRef] = shared.UnionString(in.CheckoutRef)
	}
	req.Metadata = dodopayments.F(md)
	req.Customization = dodopayments.F(customization(in.Theme))
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
	return c.eventFromSubscription(subscriptionPayload{
		Currency:              string(sub.Currency),
		CustomerID:            sub.Customer.CustomerID,
		Metadata:              stringMetadata(sub.Metadata),
		NextBillingDate:       &sub.NextBillingDate,
		PreviousBillingDate:   &sub.PreviousBillingDate,
		ProductID:             sub.ProductID,
		RecurringPreTaxAmount: sub.RecurringPreTaxAmount,
		Status:                string(sub.Status),
		SubscriptionID:        sub.SubscriptionID,
	}), nil
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
	if payment.SubscriptionID == "" {
		return corebilling.SubscriptionEvent{}, nil
	}
	event, err := c.FetchSubscription(ctx, payment.SubscriptionID)
	if err != nil {
		return corebilling.SubscriptionEvent{}, err
	}
	// The payment names a subscription, so one exists: a zero here is a shape change,
	// not "not yet". Passed through, it would leave the buyer polling for good.
	if event.IsZero() {
		return corebilling.SubscriptionEvent{}, fmt.Errorf(
			"dodo: subscription %s decoded to nothing", payment.SubscriptionID)
	}
	// Dodo calls a checkout's metadata the PAYMENT's and pug reads both off the
	// SUBSCRIPTION; fall back rather than depend on whether it propagates.
	md := stringMetadata(payment.Metadata)
	if event.OrgID == "" {
		event.OrgID = md[metadataOrgID]
	}
	if event.CheckoutRef == "" {
		event.CheckoutRef = md[metadataCheckoutRef]
	}
	return event, nil
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
