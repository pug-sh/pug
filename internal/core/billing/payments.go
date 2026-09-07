package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

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

// Purchasable reports whether this deployment sells anything to this org at all.
// It gates the buy button as a whole; whether a PARTICULAR tier can be bought is
// PlanOption.Purchasable, which shares checkoutProduct with the refusal.
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
	// A floor is never sold and a retired tier is never handed to somebody new.
	// Dodo's product map already excludes both, so today this refuses nothing the
	// lookup below would not -- but that exclusion is a PROVIDER's, and core must
	// not assume the next one builds its map the same way. The rule belongs on
	// this side of the seam.
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

	sessionID, url, err := s.payments.Provider.CreateCheckoutSession(ctx, CheckoutInput{
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

// PlanOption is a tier as this deployment sells it, distinct from Entitlement,
// which is a tier as one org holds it. A negotiated quota or display name never
// appears here.
type PlanOption struct {
	Slug        string
	DisplayName string
	Currency    string

	// nil means no list price: the custom tier, whose price lives in the provider.
	PriceCents *int64
	// nil means no quota of its own: the custom tier again, whose quota comes from
	// the org's row.
	IncludedEvents *int64
	// How far back the tier keeps history. nil is the custom tier, whose retention
	// is whatever its deal recorded -- never a zero.
	RetentionDays *int64

	Purchasable bool
}

// PlanOptions is the sellable catalog for one org. The floors are excluded --
// nobody buys Free -- and a retired tier is excluded too, since it is kept only
// so existing holders keep resolving.
//
// Custom is included only for the org whose row records a product, which is what
// makes a negotiated deal buyable from the dashboard without a link ever leaving
// our hands.
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
// -- or names none at all. Distinct from a failed lookup: it is the guard that
// makes a client-supplied session id safe to act on.
var ErrCheckoutNotForOrg = errors.New("billing: this checkout does not belong to this org")

// ErrCheckoutFailed is a checkout the provider says will not settle -- a declined
// card, most often. Distinct from a zero event, which means "not yet": without
// it a decline is indistinguishable from a slow payment and the buyer is told to
// keep waiting for money that will never arrive.
var ErrCheckoutFailed = errors.New("billing: this checkout did not complete")

// ErrSubscriptionNotFound is a subscription the provider no longer knows. A
// finding for the reconcile pass rather than a read failure: retrying it every
// run would hold the CronJob red forever over a row that is never coming back.
var ErrSubscriptionNotFound = errors.New("billing: the provider does not know this subscription")

// ConfirmCheckout verifies one checkout against the provider and writes its
// subscription through the same CAS the webhook uses. Both stamp the same
// column, so neither can overwrite the other's newer row -- though the webhook
// stamps the provider's signed time and this stamps pug's, which agree only as
// well as the two clocks do.
//
// It reports false, nil when the provider has no subscription for the session
// yet. That is the ordinary answer for a buyer who got back before the payment
// settled, and the caller should keep waiting rather than report a failure.
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

	// The whole reason a client may hand us a session id. The webhook can fall back
	// to attribution by customer, because nobody chose which delivery arrived; here
	// the caller chose the id, so only the org_id pug itself wrote at checkout will
	// do, and an absent one is a refusal rather than a lookup.
	if event.OrgID == "" || event.OrgID != orgID {
		slog.WarnContext(ctx, "refusing a checkout confirmation for another org",
			slog.String("org_id", orgID), slog.String("checkout_org_id", event.OrgID),
			slog.String("provider_sub_id", event.ProviderSubID))
		return false, ErrCheckoutNotForOrg
	}
	// A real subscription with no status at all means the provider's schema and
	// pug's mapping have diverged. Rare, but the buyer is left waiting on it, so it
	// must not pass silently the way "not settled yet" does.
	if event.Status == "" {
		err := errors.New("billing: confirmed subscription carries no status")
		slog.ErrorContext(ctx, "confirmed checkout carries no status", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("provider_sub_id", event.ProviderSubID))
		telemetry.RecordError(ctx, err)
		return false, err
	}
	// Loud rather than silent: the customer has paid and pug cannot place it. The
	// webhook's disposition for both of these is to store and alert, and a person
	// has to act either way -- but here somebody is waiting for the answer, so it
	// is returned as well as logged.
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
	if _, err := s.applyReconciledSubscription(ctx, provider, orgID, event, rec, now); err != nil {
		return false, err
	}
	// Reported from the PROVIDER's state rather than from whether our write landed.
	// The CAS skips the write when a webhook already stored a newer one, and telling
	// a buyer to keep waiting for the plan they already hold is the exact failure
	// this path exists to remove. The one refusal that is NOT a no-op -- a second
	// live subscription -- comes back as an error above rather than as a skip. A
	// subscription still `pending` writes its row and grants nothing, so it is not a
	// confirmation either.
	return event.Status.Live(), nil
}
