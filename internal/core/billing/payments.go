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
	// ErrNotPurchasable is a plan with nothing to check out against: no mandate
	// product configured, or custom for an org with no deal recorded.
	ErrNotPurchasable = errors.New("billing: this plan has no product to check out against")
	// ErrNoCustomer is a portal asked for by an org that has never checked out.
	ErrNoCustomer = errors.New("billing: this org has no payments customer")
	// ErrCurrencyNotSupported: pug sells and stores USD, and a currency it cannot
	// render honestly must not become a number on a page.
	ErrCurrencyNotSupported = errors.New("billing: only USD subscriptions are supported")
	// ErrCheckoutNotForOrg is a session id whose subscription names a different org
	// or none, or carries no ref pug minted for this org — the guard that makes a
	// client-supplied session id safe to act on.
	ErrCheckoutNotForOrg = errors.New("billing: this checkout does not belong to this org")
)

// Currency is the one pug sells in, enforced at the webhook boundary so
// multi-currency changes here — and renames amount_cents with it.
const Currency = "USD"

// Payments is the provider wiring. Nil means no provider, which is legal.
type Payments struct {
	Provider PaymentProvider
	// MandateProduct is the one product every org authorizes against, deal or not.
	// There is nothing per plan to create, so there is no map to disagree with.
	MandateProduct string
	// ReturnURL is where the provider sends a buyer after checkout: the dashboard's
	// own billing page, never a provider page.
	ReturnURL string
}

func (p *Payments) configured() bool { return p != nil && p.Provider != nil }

// Purchasable reports whether this deployment can take a mandate at all. It is
// per deployment rather than per org now: one product backs every checkout.
func (s *Service) Purchasable() bool {
	return s.billingEnabled && s.payments.configured() && s.payments.MandateProduct != ""
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

// checkoutSlug admits what may be pinned at a checkout: the current card, or
// custom for an org whose row records a deal. A retired card is not sold, though
// a holder keeps it without ever buying again.
func (s *Service) checkoutSlug(rec Record, slug string) error {
	if !s.payments.configured() {
		return ErrNoProvider
	}
	if s.payments.MandateProduct == "" {
		return ErrNotPurchasable
	}
	switch slug {
	case SlugCustom:
		if _, ok := rec.Terms(); !ok {
			return ErrNotPurchasable
		}
	case CurrentCard().Slug:
	default:
		if _, ok := CardBySlug(slug); !ok {
			return ErrPlanNotFound
		}
		return ErrNotPurchasable
	}
	return nil
}

// Checkout is what a caller knows: which org buys on which terms, and who from
// which page. The product, the ref and the return URL are the service's to derive.
type Checkout struct {
	OrgID    string
	PlanSlug string
	// Email and Name pre-fill the provider's form. Both may be empty.
	Email string
	Name  string
	Theme CheckoutTheme
}

// CreateCheckoutSession opens a provider checkout and returns the URL to send the
// buyer to. The slug is what gets pinned; the product is the same either way, and
// no amount is named — pug prices each period and charges that.
func (s *Service) CreateCheckoutSession(
	ctx context.Context, in Checkout,
) (sessionID, checkoutURL string, err error) {
	orgID, planSlug := in.OrgID, in.PlanSlug
	if !s.billingEnabled || !s.payments.configured() {
		return "", "", ErrNoProvider
	}
	rec, err := s.StoredRecord(ctx, orgID)
	if err != nil {
		return "", "", err
	}
	if err := s.checkoutSlug(rec, planSlug); err != nil {
		return "", "", err
	}

	// Before the provider call: the ref travels in the checkout's metadata. An
	// orphan row is pruned; a missing one leaves a real purchase unattributable.
	ref, err := newCheckoutRef()
	if err != nil {
		return "", "", err
	}
	// The slug is stored with it, so the card in force when the buyer agreed is the
	// card they are billed on even if it retires while the checkout is open.
	if err := s.write().CreateBillingCheckoutSession(ctx, dbwrite.CreateBillingCheckoutSessionParams{
		OrgID:    orgID,
		PlanSlug: planSlug,
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
		CustomerEmail: in.Email,
		CustomerName:  in.Name,
		Theme:         in.Theme,
		OrgID:         orgID,
		ProductID:     s.payments.MandateProduct,
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

// PlanOption is a plan as this deployment sells it, distinct from Entitlement,
// which is a plan as ONE ORG holds it.
type PlanOption struct {
	Slug        string
	DisplayName string
	Currency    string

	// Exactly one of these is set: what the option costs, priced per event.
	Card  *RateCard
	Terms *CustomTerms

	Purchasable bool
}

// PlanOptions is the current card, plus custom for the org whose row records a
// deal. A retired card is never offered — its holders keep it without buying.
func (s *Service) PlanOptions(ctx context.Context, orgID string) ([]PlanOption, error) {
	rec, err := s.StoredRecord(ctx, orgID)
	if err != nil {
		return nil, err
	}
	card := CurrentCard()
	out := []PlanOption{{
		Card:        &card,
		Currency:    card.Currency,
		DisplayName: card.DisplayName,
		Purchasable: s.Purchasable(),
		Slug:        card.Slug,
	}}
	if terms, ok := rec.Terms(); ok {
		out = append(out, PlanOption{
			Currency:    Currency,
			DisplayName: "Custom",
			Purchasable: s.Purchasable(),
			Slug:        SlugCustom,
			Terms:       &terms,
		})
	}
	return out, nil
}

// ConfirmCheckout verifies one checkout against the provider and writes its
// subscription through the same CAS the webhook uses. false, nil means the
// provider has no subscription yet — the buyer beat their own payment home.
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
	// metadata.org_id only names an org; the minted ref proves one. Pug mints one for
	// every checkout it opens, and "" matches no row, so a ref-less confirm lands here.
	session, err := s.write().GetBillingCheckoutSession(ctx,
		dbwrite.GetBillingCheckoutSessionParams{
			Provider: provider.Name(),
			Ref:      event.CheckoutRef,
		})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.ErrorContext(ctx, "failed to read a billing checkout session", slogx.Error(err),
			slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return false, err
	}
	if err != nil || session.OrgID != orgID {
		slog.WarnContext(ctx, "refusing a checkout confirmation pug did not open for this org",
			slog.String("org_id", orgID), slog.String("ref_org_id", session.OrgID),
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
	// A recurring mandate would be billed by the provider on its own schedule as
	// well as by pug's invoices; a tax-inclusive one would carve the tax out of
	// pug's amount. Refused here too, so the buyer is told rather than polling.
	if !event.OnDemand || event.TaxInclusive {
		slog.ErrorContext(ctx, "confirmed checkout is not a mandate pug can charge",
			slogx.Error(ErrSubscriptionUnapplicable),
			slog.String("org_id", orgID), slog.String("provider_sub_id", event.ProviderSubID),
			slog.Bool("on_demand", event.OnDemand), slog.Bool("tax_inclusive", event.TaxInclusive))
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
