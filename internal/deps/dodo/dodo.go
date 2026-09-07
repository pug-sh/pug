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

// One stalled connection must not eat a dashboard request's budget, nor the
// reconcile pass's 30m -- which one hung read could otherwise spend entirely.
const requestTimeout = 10 * time.Second

// Client implements billing.PaymentProvider against Dodo Payments.
type Client struct {
	api      *dodopayments.Client
	verifier *verifier
}

var _ corebilling.PaymentProvider = (*Client)(nil)

var ErrNotConfigured = errors.New("dodo: no API key configured")

// New returns nil when no API key is configured. Billing enabled with no
// provider credentials is a supported mode, not a broken one: quotas, grants and
// comped deals all work and only the buy button is missing. That is the
// self-hosted configuration.
//
// The webhook secret is separate, and needed on top of a key rather than instead
// of one: it is what mounts the route, so a deployment with a secret and no key
// gets no client and therefore no route.
func New(cfg Config, products map[string]string) (*Client, error) {
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

// CanVerify reports whether a webhook secret was configured, which is what
// decides that the route mounts at all.
func (c *Client) CanVerify() bool { return c != nil && c.verifier != nil }

func (c *Client) CreateCheckoutSession(
	ctx context.Context, in corebilling.CheckoutInput,
) (sessionID, checkoutURL string, err error) {
	req := dodopayments.CheckoutSessionRequestParam{
		ProductCart: dodopayments.F([]dodopayments.ProductItemReqParam{{
			ProductID: dodopayments.F(in.ProductID),
			Quantity:  dodopayments.F(int64(1)),
		}}),
		// Attribution. Every delivery this checkout produces carries it, which is
		// what lets the webhook place a subscription without guessing.
		Metadata: dodopayments.F(dodopayments.MetadataParam{
			metadataOrgID: shared.UnionString(in.OrgID),
		}),
	}
	if in.ReturnURL != "" {
		req.ReturnURL = dodopayments.F(in.ReturnURL)
	}
	// A new customer per checkout, never a lookup by email: one person can admin
	// two orgs, and a customer shared between them would let attribution -- which
	// falls back to the customer id -- land a delivery on the wrong tenant. The
	// flag is what forces it; Dodo's default is to match an existing email.
	req.FeatureFlags = dodopayments.F(dodopayments.CheckoutSessionFlagsParam{
		AlwaysCreateNewCustomer: dodopayments.F(true),
	})
	if in.CustomerEmail != "" {
		req.Customer = dodopayments.F[dodopayments.CustomerRequestUnionParam](
			dodopayments.NewCustomerParam{Email: dodopayments.F(in.CustomerEmail)},
		)
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

// FetchCheckoutOutcome walks one checkout session to the subscription it
// produced: session -> payment -> subscription. Dodo's session status carries a
// payment id but no subscription id, and only the subscription object carries
// the product, price, period and the org_id metadata pug attributes on -- so the
// last hop is FetchSubscription itself, which is what keeps this event identical
// to the one a delivery normalizes to.
//
// Each missing link is a zero event rather than an error: a session still
// collecting details has no payment, and a payment that is not a subscription's
// has nothing to apply. Both mean "not yet", and neither is a fault -- but a
// payment Dodo has already given up on is neither, so its status is read rather
// than dropped, or a declined card would poll forever as "not yet".
func (c *Client) FetchCheckoutOutcome(ctx context.Context, sessionID string) (corebilling.SubscriptionEvent, error) {
	session, err := c.api.CheckoutSessions.Get(ctx, sessionID)
	if err != nil {
		// A session Dodo has never heard of, or has aged out, is one it will never
		// settle -- the interface's "given up on", not its "not yet". Reported as a
		// dead checkout so the buyer is told, rather than polling a 500 forever.
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
	// Dodo's own docs call a checkout's metadata the PAYMENT's, and pug reads
	// org_id off the SUBSCRIPTION. Whether it propagates between the two is the
	// provider's business, so fall back to the payment rather than let confirming a
	// buyer depend on that answer.
	if event.OrgID == "" {
		event.OrgID = stringMetadata(payment.Metadata)[metadataOrgID]
	}
	return event, nil
}

// terminalIntent reports a payment Dodo will not carry further. Deliberately a
// short allowlist of the states that are over: everything else -- processing, an
// unfinished 3DS challenge, a word Dodo adds later -- stays "not yet", so an
// unknown state can only ever delay the answer, never invent a failure.
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

// notFound separates "the provider does not have this" from "the provider could
// not be reached". Everything above treats the first as a finding and the second
// as an outage, and without the status code they are the same error.
func notFound(err error) bool {
	var apiErr *dodopayments.Error
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}
