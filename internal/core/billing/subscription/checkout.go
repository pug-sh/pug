package subscription

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
	"github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/core/billing/entitlement"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
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
	// ErrNotPurchasable is a plan with no product to check out against — a catalog
	// plan with no product configured, one off sale, or a deal whose product id nobody
	// pasted on yet.
	ErrNotPurchasable = errors.New("billing: this plan has no product to check out against")
	// ErrNoCustomer is a portal asked for by an org that has never checked out.
	ErrNoCustomer = errors.New("billing: this org has no payments customer")
	// ErrCurrencyNotSupported: pug stores every subscription as USD, so a foreign
	// currency would be stored as a USD figure it never was.
	ErrCurrencyNotSupported = errors.New("billing: only USD subscriptions are supported")
	// ErrCheckoutNotForOrg is a session id whose subscription names a different org
	// or none, or carries no ref pug minted for this org — the guard that makes a
	// client-supplied session id safe to act on.
	ErrCheckoutNotForOrg = errors.New("billing: this checkout does not belong to this org")
)

// takesMoney is the guard the dashboard's money paths share: billing switched
// on, read through the entitlement service, and a provider wired.
// CreateCheckoutSession, CreatePortalSession and ConfirmCheckout refuse on it with
// billing.ErrNoProvider; Purchasable, Manageable and each PlanOption report it.
// HandleDelivery and Reconcile ask only for a provider: the webhook mirrors the
// provider whether or not the switch is on, and the reconcile CronJob builds no
// provider while it is off.
func (s *Service) takesMoney() bool {
	return s.entitlements.BillingEnabled() && s.payments.Configured()
}

// Purchasable reports whether this deployment sells anything to this org at all:
// any of its offers would open a checkout. Per plan it is PlanOption.Purchasable.
func (s *Service) Purchasable(rec entitlement.Record) bool {
	for _, o := range s.offers(rec) {
		if o.Purchasable {
			return true
		}
	}
	return false
}

// Manageable reports whether a portal session would open, from the SAME lookup
// CreatePortalSession refuses on: a cancelled org still wants its invoices.
func (s *Service) Manageable(ctx context.Context, orgID string) bool {
	if !s.takesMoney() {
		return false
	}
	customerID, err := s.anyProviderCustomer(ctx, orgID)
	// A failed read is already logged; false renders no button, the safe direction.
	return err == nil && customerID != ""
}

// checkoutProduct resolves the product a slug is bought against. A deal's is the
// org's own, so a negotiated deal is buyable without pug creating one.
func (s *Service) checkoutProduct(rec entitlement.Record, slug string) (string, error) {
	if !s.payments.Configured() {
		return "", billing.ErrNoProvider
	}
	if slug == entitlement.SlugCustom {
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

// Checkout is what a caller knows: which org buys which plan, and who from which
// page. The product, the ref and the return URL are the service's to derive.
type Checkout struct {
	OrgID    string
	PlanSlug string
	// Email and Name pre-fill the provider's form. Both may be empty.
	Email string
	Name  string
	Theme billing.CheckoutTheme
}

// CreateCheckoutSession opens a provider checkout for one plan, or the org's own
// deal, and returns the URL to send the buyer to. The amount lives on the product,
// never here.
func (s *Service) CreateCheckoutSession(
	ctx context.Context, in Checkout,
) (sessionID, checkoutURL string, err error) {
	orgID, planSlug := in.OrgID, in.PlanSlug
	if !s.takesMoney() {
		return "", "", billing.ErrNoProvider
	}
	// Before the read, whatever the org holds: free is a state and never sold, and a
	// slug that is neither a state nor a plan is the caller's mistake.
	switch _, known := entitlement.PlanBySlug(planSlug); {
	case planSlug == entitlement.SlugFree:
		return "", "", ErrNotPurchasable
	case !known && planSlug != entitlement.SlugCustom:
		return "", "", entitlement.ErrPlanNotFound
	}

	rec, err := s.entitlements.StoredRecord(ctx, orgID)
	if err != nil {
		return "", "", err
	}
	// Only what the org is offered: free never is, nor a plan off sale, nor a deal
	// with no product.
	var productID string
	for _, o := range s.offers(rec) {
		if o.Slug == planSlug && o.Purchasable {
			productID = o.productID
		}
	}
	if productID == "" {
		return "", "", ErrNotPurchasable
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

	sessionID, url, err := s.payments.Provider.CreateCheckoutSession(ctx, billing.CheckoutInput{
		CheckoutRef:   ref,
		CustomerEmail: in.Email,
		CustomerName:  in.Name,
		Theme:         in.Theme,
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
	if !s.takesMoney() {
		return "", billing.ErrNoProvider
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

// planForProduct maps a delivery's product onto the plan it buys: a catalog slug,
// or custom for the org's own deal, whose terms then come from its row.
func (s *Service) planForProduct(productID string, rec entitlement.Record) (string, error) {
	if productID == "" {
		return "", ErrNotPurchasable
	}
	for slug, id := range s.payments.ProductBySlug {
		if id == productID {
			return slug, nil
		}
	}
	if rec.ProviderProductID != "" && rec.ProviderProductID == productID {
		return entitlement.SlugCustom, nil
	}
	return "", fmt.Errorf("%w: product %s", ErrNotPurchasable, productID)
}

// PlanOption is a plan as this deployment sells it, distinct from
// entitlement.Entitlement, which is a plan as ONE ORG holds it. Quantities only: the
// rates live on the provider's product.
type PlanOption struct {
	Slug        string
	DisplayName string
	// The plan's free allowance. nil for a deal: its terms are the org's entitlement
	// once bought, its base plan's unless its row overrides them.
	IncludedEvents *int64
	// nil for a deal again — never a zero.
	RetentionDays *int64

	Purchasable bool
	// What a checkout opens against, set exactly when Purchasable. Unexported, so no
	// product id leaves the package, let alone reaches the wire.
	productID string
}

// PlanOptions is what this org is offered: every plan on sale, and its own deal when
// its row names a product.
func (s *Service) PlanOptions(ctx context.Context, orgID string) ([]PlanOption, error) {
	rec, err := s.entitlements.StoredRecord(ctx, orgID)
	if err != nil {
		return nil, err
	}
	return s.offers(rec), nil
}

// offers is the one list PlanOptions shows, Purchasable summarizes and
// CreateCheckoutSession opens a checkout from, so a button and the call behind it
// cannot disagree.
func (s *Service) offers(rec entitlement.Record) []PlanOption {
	return s.offersFrom(entitlement.Plans(), rec)
}

// offersFrom takes the plans as a parameter so the retired-plan rule stays testable
// while the real catalog holds one plan on sale.
func (s *Service) offersFrom(plans []entitlement.Plan, rec entitlement.Record) []PlanOption {
	var out []PlanOption
	for _, plan := range plans {
		// The product map keeps a retired plan mapped, so its holders' renewals still
		// resolve (see app/payments). This is what keeps it off sale, and the map is the
		// wiring's rule besides: core must not assume the next wiring builds it the same
		// way.
		if !plan.OnSale() {
			continue
		}
		out = append(out, s.offer(rec, PlanOption{
			DisplayName:    plan.DisplayName,
			IncludedEvents: i64(plan.FreeEvents),
			RetentionDays:  i64(plan.RetentionDays),
			Slug:           plan.Slug,
		}))
	}
	// A state, not a catalog plan: the org's own row supplies its product.
	if rec.ProviderProductID != "" {
		out = append(out, s.offer(rec, PlanOption{
			DisplayName: entitlement.CustomDisplayName,
			Slug:        entitlement.SlugCustom,
		}))
	}
	return out
}

// offer marks o purchasable when a checkout for it would open: per plan, not per
// org, since a deployment can configure a product for one plan and not another, and
// a button that cannot work is worse than no button.
func (s *Service) offer(rec entitlement.Record, o PlanOption) PlanOption {
	if !s.takesMoney() {
		return o
	}
	if id, err := s.checkoutProduct(rec, o.Slug); err == nil {
		o.Purchasable, o.productID = true, id
	}
	return o
}

func i64(v int64) *int64 { return &v }

// ConfirmCheckout verifies one checkout against the provider and writes its
// subscription through the same CAS the webhook uses. false, nil means the
// provider has no subscription yet — the buyer beat their own payment home.
func (s *Service) ConfirmCheckout(ctx context.Context, orgID, sessionID string, now time.Time) (bool, error) {
	if !s.takesMoney() {
		return false, billing.ErrNoProvider
	}
	provider := s.payments.Provider

	event, err := provider.FetchCheckoutOutcome(ctx, sessionID)
	if err != nil {
		// A decline is an ordinary buyer outcome; only a failed read is a fault.
		if errors.Is(err, billing.ErrCheckoutFailed) {
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
	// metadata.org_id only names an org; the minted ref proves one. Pug mints one for
	// every checkout it opens, and "" matches no row, so a ref-less confirm lands here.
	refOrg, err := dbread.New(s.pgW).GetBillingCheckoutSessionOrgID(ctx,
		dbread.GetBillingCheckoutSessionOrgIDParams{
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
	if cur := normalizeCurrency(event.Currency); cur != billing.Currency {
		slog.ErrorContext(ctx, "confirmed checkout is billed in an unsupported currency",
			slogx.Error(ErrCurrencyNotSupported), slog.String("org_id", orgID),
			slog.String("currency", cur), slog.String("provider_sub_id", event.ProviderSubID))
		telemetry.RecordError(ctx, ErrCurrencyNotSupported)
		return false, ErrCurrencyNotSupported
	}
	rec, err := s.entitlements.StoredRecord(ctx, orgID)
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

// anyProviderCustomer resolves the customer the portal is opened for. Checkout
// is what leaves one behind, so a free or comped org has none.
func (s *Service) anyProviderCustomer(ctx context.Context, orgID string) (string, error) {
	if !s.payments.Configured() {
		return "", billing.ErrNoProvider
	}
	// The write pool: a lagging replica would hide "Manage billing" from a customer
	// who has just paid.
	row, err := dbread.New(s.pgW).GetLatestBillingSubscription(ctx,
		dbread.GetLatestBillingSubscriptionParams{
			OrgID:    orgID,
			Provider: s.payments.Provider.Name(),
		})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNoCustomer
		}
		slog.ErrorContext(ctx, "failed to read the billing subscription for a portal session", slogx.Error(err),
			slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return "", err
	}
	if row.ProviderCustomerID == "" {
		return "", ErrNoCustomer
	}
	return row.ProviderCustomerID, nil
}
