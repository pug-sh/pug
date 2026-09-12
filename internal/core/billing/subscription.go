package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/slogx"
)

// ErrSubscriptionUnreadable is a stored status this build has no word for. It is
// not "no mandate": billing an org as if it had none writes the invoice off.
var ErrSubscriptionUnreadable = errors.New("billing: the subscription holds a status pug does not know")

// liveSubscription reads the one row that can be charged. No row is the
// ordinary answer, so it returns nil rather than an error.
func (s *Service) liveSubscription(ctx context.Context, orgID string) (*Subscription, error) {
	// The write pool, like every read on the money path: ConfirmCheckout writes the
	// row and GetBillingStatus reads it immediately after, which a replica loses.
	return readLiveSubscription(ctx, dbread.New(s.pgW), orgID)
}

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
		err := fmt.Errorf("%w: %q", ErrSubscriptionUnreadable, row.Status)
		slog.ErrorContext(ctx, "live subscription holds a status pug does not know", slogx.Error(err),
			slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return nil, err
	}
	return &sub, nil
}

func subscriptionFromRow(row dbread.BillingSubscription) (Subscription, bool) {
	status, ok := ParseSubStatus(row.Status)
	if !ok {
		return Subscription{}, false
	}
	return Subscription{
		CancelAtPeriodEnd:  row.CancelAtPeriodEnd,
		CreateTime:         row.CreateTime.Time,
		Currency:           row.Currency,
		CurrentPeriodEnd:   row.CurrentPeriodEnd.Time,
		CurrentPeriodStart: row.CurrentPeriodStart.Time,
		EndedAt:            row.EndedAt.Time,
		OnDemand:           row.OnDemand,
		PlanSlug:           row.PlanSlug,
		PriceCents:         row.PriceCents,
		Provider:           row.Provider,
		ProviderCustomerID: row.ProviderCustomerID,
		ProviderSubID:      row.ProviderSubID,
		Status:             status,
		UpdateTime:         row.UpdateTime.Time,
	}, true
}

// anyProviderCustomer resolves the customer the portal is opened for. Checkout
// is what leaves one behind, so a trialing, free or comped org has none.
func (s *Service) anyProviderCustomer(ctx context.Context, orgID string) (string, error) {
	if !s.payments.configured() {
		return "", ErrNoProvider
	}
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
