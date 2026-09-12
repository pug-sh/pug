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
	coreusage "github.com/pug-sh/pug/internal/core/usage"
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
	// ErrNoProvider is billing running with no payments credentials.
	ErrNoProvider = errors.New("billing: no payments provider is configured")
	// ErrNotPurchasable is a plan with nothing to check out against: no mandate
	// product configured, or custom for an org with no deal recorded.
	ErrNotPurchasable = errors.New("billing: this plan has no product to check out against")
	ErrNoCustomer     = errors.New("billing: this org has no payments customer")
	// ErrNoMandate is a payment method removal for an org that has none.
	ErrNoMandate = errors.New("billing: this org has no live payment method")
	// ErrFinalPeriodUnsettled is the final invoice not reaching a settled charge.
	// The mandate is left live, because cancelling it turns a retryable decline
	// into a write-off.
	ErrFinalPeriodUnsettled = errors.New("billing: the final period could not be billed; the payment method is unchanged")
	// ErrCurrencyNotSupported: pug sells and stores USD, and a currency it cannot
	// render honestly must not become a number on a page.
	ErrCurrencyNotSupported = errors.New("billing: only USD subscriptions are supported")
	// ErrCheckoutNotForOrg is a session id whose subscription names a different org
	// or none, or carries no ref pug minted for this org.
	ErrCheckoutNotForOrg = errors.New("billing: this checkout does not belong to this org")
)

const Currency = "USD"

// Payments is the provider wiring. Nil means no provider, which is legal.
type Payments struct {
	Provider PaymentProvider
	// MandateProduct is the one on-demand product every org authorizes against.
	// Its stored price is never charged.
	MandateProduct string
	// ReturnURL is where the provider sends a buyer after checkout: the dashboard's
	// own billing page, never a provider page.
	ReturnURL string
}

func (p *Payments) configured() bool { return p != nil && p.Provider != nil }

// Purchasable reports whether this deployment can take a mandate at all.
func (s *Service) Purchasable() bool {
	return s.cfg.Enabled && s.payments.configured() && s.payments.MandateProduct != ""
}

// Manageable reports whether a portal session would open, from the SAME lookup
// CreatePortalSession refuses on: a cancelled org still wants its invoices.
func (s *Service) Manageable(ctx context.Context, orgID string) bool {
	if !s.cfg.Enabled || !s.payments.configured() {
		return false
	}
	customerID, err := s.anyProviderCustomer(ctx, orgID)
	return err == nil && customerID != ""
}

type Checkout struct {
	OrgID    string
	PlanSlug string
	Email    string
	Name     string
	Theme    CheckoutTheme
}

// CreateCheckoutSession opens a mandate-only checkout and returns the URL to send
// the buyer to. The slug is what gets pinned; the product is the same either way.
func (s *Service) CreateCheckoutSession(
	ctx context.Context, in Checkout,
) (sessionID, checkoutURL string, err error) {
	orgID, planSlug := in.OrgID, in.PlanSlug
	if !s.cfg.Enabled || !s.payments.configured() {
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

// checkoutSlug admits the current card, or custom for an org whose row records
// a deal. A retired card is not sold; a pinned holder keeps it without buying.
func (s *Service) checkoutSlug(rec Record, slug string) error {
	if s.payments.MandateProduct == "" {
		return ErrNotPurchasable
	}
	switch {
	case slug == SlugCustom:
		if _, ok := rec.Terms(); !ok {
			return ErrNotPurchasable
		}
	case slug == CurrentCard().Slug:
	case slug == SlugFree || slug == SlugTrial:
		return ErrNotPurchasable
	default:
		if _, ok := CardBySlug(slug); !ok {
			return ErrPlanNotFound
		}
		return ErrNotPurchasable
	}
	return nil
}

func (s *Service) CreatePortalSession(ctx context.Context, orgID string) (string, error) {
	if !s.cfg.Enabled || !s.payments.configured() {
		return "", ErrNoProvider
	}
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

func normalizeCurrency(v string) string { return strings.ToUpper(strings.TrimSpace(v)) }

// PlanOption is a plan as this deployment sells it, distinct from Entitlement,
// which is a plan as ONE ORG holds it.
type PlanOption struct {
	Slug        string
	DisplayName string
	Currency    string
	Card        *RateCard
	Terms       *CustomTerms
	// nil is a fee-only deal; a card's is its free allowance.
	IncludedEvents *int64
	RetentionDays  *int64
	Purchasable    bool
}

// PlanOptions is the current card, plus custom for the org whose row records a deal.
func (s *Service) PlanOptions(ctx context.Context, orgID string) ([]PlanOption, error) {
	rec, err := s.StoredRecord(ctx, orgID)
	if err != nil {
		return nil, err
	}
	card := CurrentCard()
	out := []PlanOption{{
		Slug:           card.Slug,
		DisplayName:    card.DisplayName,
		Currency:       card.Currency,
		Card:           &card,
		IncludedEvents: i64(card.FreeBlocks * card.BlockEvents),
		RetentionDays:  i64(card.RetentionDays),
		Purchasable:    s.Purchasable(),
	}}
	if terms, ok := rec.Terms(); ok {
		opt := PlanOption{
			Slug:          SlugCustom,
			DisplayName:   "Custom",
			Currency:      Currency,
			Terms:         &terms,
			RetentionDays: i64(RetentionDays),
			Purchasable:   s.Purchasable(),
		}
		if terms.BlockRateCents > 0 {
			opt.IncludedEvents = i64(terms.IncludedEvents)
		}
		out = append(out, opt)
	}
	return out, nil
}

// ConfirmCheckout verifies one checkout against the provider and writes its
// mandate through the same CAS the webhook uses. false, nil means the provider
// has no subscription yet — the buyer beat their own authorization home.
func (s *Service) ConfirmCheckout(ctx context.Context, orgID, sessionID string, now time.Time) (bool, error) {
	if !s.cfg.Enabled || !s.payments.configured() {
		return false, ErrNoProvider
	}
	provider := s.payments.Provider

	event, err := provider.FetchCheckoutOutcome(ctx, sessionID)
	if err != nil {
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
	// metadata.org_id only names an org; the minted ref proves one.
	session, err := s.write().GetBillingCheckoutSession(ctx, dbwrite.GetBillingCheckoutSessionParams{
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
	if event.Status == "" {
		slog.ErrorContext(ctx, "confirmed checkout carries no status",
			slogx.Error(ErrSubscriptionUnapplicable),
			slog.String("org_id", orgID), slog.String("provider_sub_id", event.ProviderSubID))
		telemetry.RecordError(ctx, ErrSubscriptionUnapplicable)
		return false, ErrSubscriptionUnapplicable
	}
	if cur := normalizeCurrency(event.Currency); cur != Currency {
		slog.ErrorContext(ctx, "confirmed checkout is billed in an unsupported currency",
			slogx.Error(ErrCurrencyNotSupported), slog.String("org_id", orgID),
			slog.String("currency", cur), slog.String("provider_sub_id", event.ProviderSubID))
		telemetry.RecordError(ctx, ErrCurrencyNotSupported)
		return false, ErrCurrencyNotSupported
	}
	if _, err := s.applySubscription(ctx, provider, orgID, event, now); err != nil {
		return false, err
	}
	// From the PROVIDER's state, not from whether our write landed: the CAS skips
	// the write when a webhook already stored a newer row.
	return event.Status.Live(), nil
}

// RemovePaymentMethod is pug's own cancellation, in the order the portal cannot
// promise: close the period to date, charge it while the mandate is live, then
// cancel. Days the meter has not finalized are the write-off.
func (s *Service) RemovePaymentMethod(ctx context.Context, orgID string, now time.Time) error {
	if !s.cfg.Enabled || !s.payments.configured() {
		return ErrNoProvider
	}
	provider := s.payments.Provider
	sub, err := s.liveSubscription(ctx, orgID)
	if err != nil {
		return err
	}
	if sub == nil || sub.Provider != provider.Name() {
		return ErrNoMandate
	}

	// chargeOne reports the outcome only through the report, so the counters are
	// the check: cancel on anything but a settled charge and the final period is
	// written off, with the RPC reporting success.
	var report InvoiceReport
	inv, err := s.closeCurrentPeriod(ctx, orgID, sub, coreusage.FloorDayUTC(now.Add(-s.cfg.Grace())), now, &report)
	if err != nil {
		return err
	}
	if inv != nil && inv.Status == InvoiceOpen {
		if err := s.chargeOne(ctx, provider, *inv, now, &report); err != nil {
			return err
		}
	}
	if report.Held > 0 || report.Unpriceable > 0 || (inv != nil && inv.Status == InvoiceOpen && report.Charged == 0) {
		slog.WarnContext(ctx, "the final period did not settle; leaving the mandate live",
			slog.String("org_id", orgID), slog.Int("held", report.Held),
			slog.Int("unpriceable", report.Unpriceable), slog.Int("declined", report.Declined),
			slog.Int("ambiguous", report.Ambiguous), slog.Int("mandate_gone", report.MandateGone))
		return ErrFinalPeriodUnsettled
	}

	if err := provider.CancelSubscription(ctx, sub.ProviderSubID); err != nil {
		slog.ErrorContext(ctx, "failed to cancel a mandate", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("provider_sub_id", sub.ProviderSubID))
		telemetry.RecordError(ctx, err)
		return err
	}
	// Mirror the cancellation now rather than waiting for the webhook, so the
	// dashboard the buyer is looking at agrees with what they just did.
	event, err := provider.FetchSubscription(ctx, sub.ProviderSubID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to re-read a cancelled mandate", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("provider_sub_id", sub.ProviderSubID))
		telemetry.RecordError(ctx, err)
		return err
	}
	if event.IsZero() {
		return nil
	}
	_, err = s.applySubscription(ctx, provider, orgID, event, now)
	return err
}
