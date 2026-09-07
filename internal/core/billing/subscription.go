package billing

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/slogx"
)

// liveSubscription reads the one row that can supply a plan. No row is the
// ordinary answer, so it returns nil rather than an error.
func (s *Service) liveSubscription(ctx context.Context, orgID string) (*Subscription, error) {
	// The write pool, like every read on the money path: ConfirmCheckout writes the
	// row and GetBillingStatus reads it immediately after, which a replica loses.
	return readLiveSubscription(ctx, dbread.New(s.pgW), orgID)
}

// readLiveSubscription is the same read against a caller's handle, so Clear can
// take it through its own locked tx.
func readLiveSubscription(ctx context.Context, r *dbread.Queries, orgID string) (*Subscription, error) {
	row, err := r.GetLiveBillingSubscription(ctx, orgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		slog.ErrorContext(ctx, "failed to read the live billing subscription", slogx.Error(err),
			slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return nil, err
	}
	sub, ok := subscriptionFromRow(row)
	if !ok {
		// The query already filtered to active/past_due, so an unparsable word means a
		// writer stored a status pug cannot name. Not live is the safe reading.
		slog.ErrorContext(ctx, "live subscription holds a status pug does not know",
			slog.String("org_id", orgID), slog.String("status", row.Status))
		return nil, nil
	}
	return &sub, nil
}

func subscriptionFromRow(row dbread.BillingSubscription) (Subscription, bool) {
	status, ok := ParseSubStatus(row.Status)
	if !ok {
		return Subscription{}, false
	}
	return Subscription{
		Currency:           row.Currency,
		CurrentPeriodEnd:   row.CurrentPeriodEnd.Time,
		CurrentPeriodStart: row.CurrentPeriodStart.Time,
		PlanSlug:           row.PlanSlug,
		PriceCents:         row.PriceCents,
		ProviderCustomerID: row.ProviderCustomerID,
		ProviderSubID:      row.ProviderSubID,
		Status:             status,
	}, true
}

// anyProviderCustomer resolves the customer the portal is opened for. Checkout
// is what leaves one behind, so a trialing, free or comped org has none.
func (s *Service) anyProviderCustomer(ctx context.Context, orgID string) (string, error) {
	if !s.payments.configured() {
		return "", ErrNoProvider
	}
	// The write pool, for liveSubscription's reason: a lagging replica would hide
	// "Manage billing" from a customer who has just paid.
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
