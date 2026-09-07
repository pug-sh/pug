package billing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
)

// newCheckoutRef returns the token every delivery is attributed on. Unguessable is
// the whole property: a predictable one lands a purchase on another org.
func newCheckoutRef() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return hex.EncodeToString(b), nil
}

var (
	// ErrNoProvider is billing running with no payments credentials: the
	// self-hosted mode, where only the buy button is missing.
	ErrNoProvider = errors.New("billing: no payments provider is configured")
	// ErrNotPurchasable is a plan with no product to check out against -- an
	// unconfigured catalog tier, or a deal whose product id nobody pasted on yet.
	ErrNotPurchasable = errors.New("billing: this plan has no product to check out against")
	// ErrNoCustomer is a portal asked for by an org that has never checked out.
	ErrNoCustomer = errors.New("billing: this org has no payments customer")
	// ErrCurrencyNotSupported: pug sells and stores USD, and a currency it cannot
	// render honestly must not become a number on a page.
	ErrCurrencyNotSupported = errors.New("billing: only USD subscriptions are supported")
)

// Currency is the one pug sells in, enforced at the webhook boundary so
// multi-currency changes here -- and renames price_cents with it.
const Currency = "USD"

// Payments is the provider wiring. Nil means no provider, which is legal.
type Payments struct {
	Provider PaymentProvider
	// ProductBySlug is checkout's direction, SlugByProduct is the webhook's. Derive
	// the second from the first: a one-way slug takes money and rejects the delivery.
	ProductBySlug map[string]string
	SlugByProduct map[string]string
	// ReturnURL is where the provider sends a buyer after checkout: the dashboard's
	// own billing page, never a provider page.
	ReturnURL string
}

func (p *Payments) configured() bool { return p != nil && p.Provider != nil }

// Purchasable reports whether this deployment sells anything to this org at all.
// Per tier it is PlanOption.Purchasable, which shares checkoutProduct with it.
func (s *Service) Purchasable(rec Record) bool {
	if !s.billingEnabled || !s.payments.configured() {
		return false
	}
	// Either a catalog tier is on sale, or this org has a negotiated product.
	return len(s.payments.ProductBySlug) > 0 || rec.ProviderProductID != ""
}

// Manageable reports whether a portal session would open, from the SAME lookup
// CreatePortalSession refuses on: a cancelled org still wants its invoices.
func (s *Service) Manageable(ctx context.Context, orgID string) bool {
	if !s.billingEnabled || !s.payments.configured() {
		return false
	}
	customerID, err := s.anyProviderCustomer(ctx, orgID)
	// A failed read is already logged; false renders no button, the safe direction.
	return err == nil && customerID != ""
}

// checkoutProduct resolves the product a slug is bought against. The custom tier
// is the org's own, so a negotiated deal is buyable without pug creating one.
func (s *Service) checkoutProduct(rec Record, slug string) (string, error) {
	if !s.payments.configured() {
		return "", ErrNoProvider
	}
	if slug == SlugCustom {
		if rec.ProviderProductID == "" {
			return "", ErrNotPurchasable
		}
		return rec.ProviderProductID, nil
	}
	id, ok := s.payments.ProductBySlug[slug]
	if !ok || id == "" {
		return "", ErrNotPurchasable
	}
	return id, nil
}

// CreateCheckoutSession opens a provider checkout for one tier and returns the
// URL to send the buyer to. The amount lives on the product, never here.
func (s *Service) CreateCheckoutSession(
	ctx context.Context, orgID, planSlug, customerEmail string,
) (sessionID, checkoutURL string, err error) {
	if !s.billingEnabled {
		return "", "", ErrNoProvider
	}
	plan, ok := PlanBySlug(planSlug)
	if !ok {
		return "", "", ErrPlanNotFound
	}
	// The provider's product map already excludes both, but that is a PROVIDER's
	// rule; core must not assume the next one builds its map the same way.
	if plan.isFloor() || plan.Retired {
		return "", "", ErrNotPurchasable
	}

	rec, err := s.StoredRecord(ctx, orgID)
	if err != nil {
		return "", "", err
	}
	productID, err := s.checkoutProduct(rec, planSlug)
	if err != nil {
		return "", "", err
	}

	// Before the provider call: the ref travels in the checkout's metadata. An
	// orphan row is pruned; a missing one leaves a real purchase unattributable.
	ref, err := newCheckoutRef()
	if err != nil {
		return "", "", err
	}
	if err := s.write().CreateBillingCheckoutSession(ctx, dbwrite.CreateBillingCheckoutSessionParams{
		OrgID:    orgID,
		Provider: s.payments.Provider.Name(),
		Ref:      ref,
	}); err != nil {
		slog.ErrorContext(ctx, "failed to store a billing checkout session", slogx.Error(err),
			slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return "", "", err
	}

	sessionID, url, err := s.payments.Provider.CreateCheckoutSession(ctx, CheckoutInput{
		CheckoutRef:   ref,
		CustomerEmail: customerEmail,
		OrgID:         orgID,
		ProductID:     productID,
		ReturnURL:     s.payments.ReturnURL,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to create a checkout session", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("plan_slug", planSlug))
		telemetry.RecordError(ctx, err)
		return "", "", err
	}
	return sessionID, url, nil
}

// CreatePortalSession opens the provider's customer portal, where plan changes,
// card updates, invoices and cancellation live. Pug serves none of those.
func (s *Service) CreatePortalSession(ctx context.Context, orgID string) (string, error) {
	if !s.billingEnabled || !s.payments.configured() {
		return "", ErrNoProvider
	}
	// Any subscription, not only a live one: a customer whose subscription lapsed
	// still has invoices to fetch and a card to re-add.
	customerID, err := s.anyProviderCustomer(ctx, orgID)
	if err != nil {
		return "", err
	}
	url, err := s.payments.Provider.CreatePortalSession(ctx, customerID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to create a portal session", slogx.Error(err),
			slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return "", err
	}
	return url, nil
}

// normalizeCurrency runs before the currency guard so a lowercase code from a
// provider is not read as a second currency.
func normalizeCurrency(v string) string { return strings.ToUpper(strings.TrimSpace(v)) }

// planForProduct maps a delivery's product onto a catalog slug. The org's own
// product resolves to the custom tier, whose quota then comes from its row.
func (s *Service) planForProduct(productID string, rec Record) (string, error) {
	if productID == "" {
		return "", ErrNotPurchasable
	}
	if slug, ok := s.payments.SlugByProduct[productID]; ok {
		return slug, nil
	}
	if rec.ProviderProductID != "" && rec.ProviderProductID == productID {
		return SlugCustom, nil
	}
	return "", fmt.Errorf("%w: product %s", ErrNotPurchasable, productID)
}

// PlanOption is a tier as this deployment sells it, distinct from Entitlement,
// which is a tier as ONE ORG holds it.
type PlanOption struct {
	Slug        string
	DisplayName string
	Currency    string

	// nil means no list price: the custom tier, whose price lives in the provider.
	PriceCents *int64
	// nil means no quota of its own: the custom tier, whose quota comes from its row.
	IncludedEvents *int64
	// nil is the custom tier again, whose retention its deal recorded -- never a zero.
	RetentionDays *int64

	Purchasable bool
}

// PlanOptions is the sellable catalog for one org: the floors and retired tiers
// are excluded, and custom appears only for the org whose row records a product.
func (s *Service) PlanOptions(ctx context.Context, orgID string) ([]PlanOption, error) {
	rec, err := s.StoredRecord(ctx, orgID)
	if err != nil {
		return nil, err
	}

	out := make([]PlanOption, 0, len(catalog))
	for _, plan := range Plans() {
		if plan.isFloor() || plan.Retired {
			continue
		}
		if plan.Slug == SlugCustom && rec.ProviderProductID == "" {
			continue
		}
		// Per tier, not per org: a deployment can configure a product for some tiers
		// and not others, and a button that cannot work is worse than no button.
		_, err := s.checkoutProduct(rec, plan.Slug)
		out = append(out, PlanOption{
			Currency:       plan.Currency,
			DisplayName:    plan.DisplayName,
			IncludedEvents: plan.IncludedEvents,
			PriceCents:     plan.PriceCents,
			Purchasable:    s.billingEnabled && err == nil,
			RetentionDays:  plan.RetentionDays,
			Slug:           plan.Slug,
		})
	}
	return out, nil
}

// ErrCheckoutNotForOrg is a session id whose subscription names a different org
// or none, or carries a ref pug did not mint for this org -- the guard that makes
// a client-supplied session id safe to act on.
var ErrCheckoutNotForOrg = errors.New("billing: this checkout does not belong to this org")

// ErrCheckoutFailed is a checkout the provider says will not settle. Distinct
// from a zero event ("not yet"), or a decline reads as a slow payment forever.
var ErrCheckoutFailed = errors.New("billing: this checkout did not complete")

// ErrSubscriptionNotFound is a subscription the provider no longer knows: a
// finding for the reconcile pass, not a read failure worth retrying every run.
var ErrSubscriptionNotFound = errors.New("billing: the provider does not know this subscription")

// ConfirmCheckout verifies one checkout against the provider and writes its
// subscription through the same CAS the webhook uses. false, nil means the
// provider has no subscription yet -- the buyer beat their own payment home.
func (s *Service) ConfirmCheckout(ctx context.Context, orgID, sessionID string, now time.Time) (bool, error) {
	if !s.billingEnabled || !s.payments.configured() {
		return false, ErrNoProvider
	}
	provider := s.payments.Provider

	event, err := provider.FetchCheckoutOutcome(ctx, sessionID)
	if err != nil {
		// A decline is an ordinary buyer outcome; only a failed read is a fault.
		if errors.Is(err, ErrCheckoutFailed) {
			slog.InfoContext(ctx, "checkout did not settle", slogx.Error(err),
				slog.String("org_id", orgID))
			return false, err
		}
		slog.ErrorContext(ctx, "failed to read a checkout outcome", slogx.Error(err),
			slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return false, err
	}
	if event.IsZero() {
		return false, nil
	}

	// The webhook can attribute by customer because nobody chose which delivery
	// arrived; here the caller chose the id, so only pug's own org_id will do.
	if event.OrgID == "" || event.OrgID != orgID {
		slog.WarnContext(ctx, "refusing a checkout confirmation for another org",
			slog.String("org_id", orgID), slog.String("checkout_org_id", event.OrgID),
			slog.String("provider_sub_id", event.ProviderSubID))
		return false, ErrCheckoutNotForOrg
	}
	// metadata.org_id only names an org; the minted ref proves one. A ref pug never
	// stored, or one it stored against another org, is not this org's checkout.
	if event.CheckoutRef != "" {
		refOrg, err := s.write().GetBillingCheckoutSessionOrgID(ctx,
			dbwrite.GetBillingCheckoutSessionOrgIDParams{
				Provider: provider.Name(),
				Ref:      event.CheckoutRef,
			})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			slog.ErrorContext(ctx, "failed to read a billing checkout session", slogx.Error(err),
				slog.String("org_id", orgID))
			telemetry.RecordError(ctx, err)
			return false, err
		}
		if err != nil || refOrg != orgID {
			slog.WarnContext(ctx, "refusing a checkout confirmation pug did not open for this org",
				slog.String("org_id", orgID), slog.String("ref_org_id", refOrg),
				slog.String("provider_sub_id", event.ProviderSubID))
			return false, ErrCheckoutNotForOrg
		}
	}
	// A real subscription with no status means the provider's schema and pug's
	// mapping have diverged; the buyer is waiting, so it must not pass silently.
	if event.Status == "" {
		slog.ErrorContext(ctx, "confirmed checkout carries no status",
			slogx.Error(ErrSubscriptionUnapplicable),
			slog.String("org_id", orgID), slog.String("provider_sub_id", event.ProviderSubID))
		telemetry.RecordError(ctx, ErrSubscriptionUnapplicable)
		return false, ErrSubscriptionUnapplicable
	}
	// The customer has paid and pug cannot place it. A person has to act either
	// way, but somebody is waiting here, so it is returned as well as logged.
	if cur := normalizeCurrency(event.Currency); cur != Currency {
		slog.ErrorContext(ctx, "confirmed checkout is billed in an unsupported currency",
			slogx.Error(ErrCurrencyNotSupported), slog.String("org_id", orgID),
			slog.String("currency", cur), slog.String("provider_sub_id", event.ProviderSubID))
		telemetry.RecordError(ctx, ErrCurrencyNotSupported)
		return false, ErrCurrencyNotSupported
	}
	rec, err := s.StoredRecord(ctx, orgID)
	if err != nil {
		return false, err
	}
	if _, err := s.planForProduct(event.ProductID, rec); err != nil {
		slog.ErrorContext(ctx, "confirmed checkout names a product pug cannot place", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("product_id", event.ProductID),
			slog.String("provider_sub_id", event.ProviderSubID))
		telemetry.RecordError(ctx, err)
		return false, err
	}

	// The same writer reconcile uses, for the same reason: a direct read has no
	// delivery stamp, so `now` is what the CAS compares.
	if _, err := s.applySubscription(ctx, provider, orgID, event, now); err != nil {
		return false, err
	}
	// From the PROVIDER's state, not from whether our write landed: the CAS skips
	// the write when a webhook already stored a newer row, and telling a buyer to
	// wait for the plan they already hold is what this path exists to remove.
	return event.Status.Live(), nil
}
