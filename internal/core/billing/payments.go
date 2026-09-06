package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/slogx"
)

var (
	// ErrNoProvider is billing running with no payments credentials. A supported
	// mode, not a fault: quotas, grants and comped deals all work and only the buy
	// button is missing. That is the self-hosted configuration.
	ErrNoProvider = errors.New("billing: no payments provider is configured")
	// ErrNotPurchasable is a plan with no product to check out against -- an
	// unconfigured catalog tier, or a negotiated deal whose product id nobody has
	// pasted onto the org yet.
	ErrNotPurchasable = errors.New("billing: this plan has no product to check out against")
	// ErrNoCustomer is a portal asked for by an org that has never checked out.
	ErrNoCustomer = errors.New("billing: this org has no payments customer")
	// ErrCurrencyNotSupported guards section 3: pug sells in USD and stores USD,
	// and a currency it cannot render honestly must not become a number on a page.
	ErrCurrencyNotSupported = errors.New("billing: only USD subscriptions are supported")
)

// Currency is the one pug sells in. Enforced at the webhook boundary rather than
// assumed, so the day multi-currency arrives this is the single place that
// changes -- and price_cents is renamed with it, since JPY has no cents and the
// name and the constraint fall together or not at all.
const Currency = "USD"

// Payments is the provider wiring. Nil means no provider, which is legal.
type Payments struct {
	Provider PaymentProvider
	// ProductBySlug is checkout's direction, SlugByProduct is the webhook's. Both
	// are built from one source so they cannot disagree.
	ProductBySlug map[string]string
	SlugByProduct map[string]string
	// ReturnURL is where the provider sends a buyer after checkout: the dashboard's
	// own billing page, never a provider page.
	ReturnURL string
}

func (p *Payments) configured() bool { return p != nil && p.Provider != nil }

// Purchasable reports whether a buy button would actually work for this org. It
// is what the dashboard renders the button from, and it is the same condition
// CreateCheckoutSession refuses on -- read from here by both, so a button that
// cannot work is impossible rather than unlikely.
func (s *Service) Purchasable(rec Record) bool {
	if !s.billingEnabled || !s.payments.configured() {
		return false
	}
	// Either a catalog tier is on sale, or this org has a negotiated product of its
	// own. Both end in a checkout; neither exposes a product id to the caller.
	return len(s.payments.ProductBySlug) > 0 || rec.ProviderProductID != ""
}

// Manageable reports whether a portal session would actually open. Read by the
// dashboard to render "Manage billing" and by CreatePortalSession to refuse, from
// the SAME lookup -- which is the point: a cancelled org has no LIVE
// subscription, so anything derived from subscription_status would hide the
// portal from exactly the org most likely to want its invoices.
func (s *Service) Manageable(ctx context.Context, orgID string) bool {
	if !s.billingEnabled || !s.payments.configured() {
		return false
	}
	customerID, err := s.anyProviderCustomer(ctx, orgID)
	// A read that failed is already logged; reporting false renders no button,
	// which is the safe direction for a status page.
	return err == nil && customerID != ""
}

// checkoutProduct resolves the product a slug is bought against. The custom tier
// is the org's own, which is what lets a negotiated deal be bought from the pug
// dashboard without pug ever creating a product.
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
// URL to send the buyer to. Nothing about the price is pug's: the amount lives
// on the product.
func (s *Service) CreateCheckoutSession(ctx context.Context, orgID, planSlug, customerEmail string) (string, error) {
	if !s.billingEnabled {
		return "", ErrNoProvider
	}
	plan, ok := PlanBySlug(planSlug)
	if !ok {
		return "", ErrPlanNotFound
	}
	// A floor is never sold and a retired tier is never handed to somebody new.
	// Dodo's product map already excludes both, so today this refuses nothing the
	// lookup below would not -- but that exclusion is a PROVIDER's, and core must
	// not assume the next one builds its map the same way. The rule belongs on
	// this side of the seam.
	if plan.isFloor() || plan.Retired {
		return "", ErrNotPurchasable
	}

	rec, err := s.StoredRecord(ctx, orgID)
	if err != nil {
		return "", err
	}
	productID, err := s.checkoutProduct(rec, planSlug)
	if err != nil {
		return "", err
	}

	url, err := s.payments.Provider.CreateCheckoutSession(ctx, CheckoutInput{
		CustomerEmail: customerEmail,
		OrgID:         orgID,
		ProductID:     productID,
		ReturnURL:     s.payments.ReturnURL,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to create a checkout session", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("plan_slug", planSlug))
		telemetry.RecordError(ctx, err)
		return "", err
	}
	return url, nil
}

// CreatePortalSession opens the provider's customer portal, which is where plan
// changes, card updates, invoices and cancellation live. Pug serves none of
// those itself.
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

// normalizeCurrency is applied before the section 3 guard so a lowercase code
// from a provider is not read as a second currency.
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
