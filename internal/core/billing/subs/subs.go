// Package subs reads billing subscriptions. Reads only: the mandate package owns
// every write to billing_subscriptions, so one writer per table survives the split.
// It is its own package to break a cycle — entitlement resolves against the live
// subscription, and mandate needs entitlement's org lock.
package subs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/slogx"
)

// Reader reads subscriptions off the writer pool, as the single Service did before
// the split.
type Reader struct {
	pgW *pgxpool.Pool
}

func NewReader(pgW *pgxpool.Pool) *Reader { return &Reader{pgW: pgW} }

// Live reads the one row that can supply a plan. No row is the ordinary answer, so
// it returns nil rather than an error.
func (r *Reader) Live(ctx context.Context, orgID string) (*billing.Subscription, error) {
	// The write pool, like every read on the money path: ConfirmCheckout writes the
	// row and GetBillingStatus reads it immediately after, which a replica loses.
	return ReadLive(ctx, dbread.New(r.pgW), orgID)
}

// ReadLive is the same read against a caller's handle, so a mutation can take it
// through its own locked tx.
func ReadLive(ctx context.Context, r *dbread.Queries, orgID string) (*billing.Subscription, error) {
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
	sub, ok := FromRow(row)
	if !ok {
		// The query already filtered to active/past_due, so an unparsable word means a
		// writer stored a status pug cannot name. Not live is the safe reading.
		err := fmt.Errorf("live subscription holds the unknown status %q", row.Status)
		slog.ErrorContext(ctx, "live subscription holds a status pug does not know", slogx.Error(err),
			slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return nil, nil
	}
	return &sub, nil
}

// FromRow maps a stored row onto the resolution type, reporting false on a status
// pug has no word for.
func FromRow(row dbread.BillingSubscription) (billing.Subscription, bool) {
	status, ok := billing.ParseSubStatus(row.Status)
	if !ok {
		return billing.Subscription{}, false
	}
	return billing.Subscription{
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
