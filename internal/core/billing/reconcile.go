package billing

import (
	"context"
	"log/slog"
	"time"

	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/slogx"
)

// reconcilePageSize bounds one read of the subscription table. The pass walks
// every row, so this is a memory bound, not a limit on what it covers.
const reconcilePageSize = 500

// DeliveryRetention is how long a processed delivery's payload is kept. It holds
// the customer's name, email and billing address -- personal data pug does not
// otherwise store -- and replay is the only thing that needs the bytes.
const DeliveryRetention = 90 * 24 * time.Hour

// ReconcileReport is what one pass found. Nothing here is auto-fixed: an
// automatic repair would be writing to the money side of the system from a
// guess. The counts are logged and returned so the caller can alert on them.
type ReconcileReport struct {
	// Rows re-read from the provider and re-applied through the same CAS the
	// webhook uses.
	Checked int
	Applied int

	// A subscription the provider says is live that pug has no row for. Counted
	// rather than listed: pug cannot enumerate the provider's subscriptions
	// without a list call the seam does not have, so this is only ever found when
	// a row pug DOES have names a customer whose subscription id has moved.
	Untracked int
	// Invariant 3, inverted: an entitlement granting a paid or custom plan with no
	// live subscription behind it. Includes the section 5.2 case of a paid custom
	// deal whose quota row nobody wrote.
	EntitledUnbilled int
	// A live subscription against a product no config key and no org row maps to
	// -- a delivery that could not be applied, which means a deploy is missing a
	// product key or an operator created a product without pasting its id.
	UnmappedProduct int
	// Rows the provider could not be read for. Distinguished from a clean pass so
	// a provider outage does not read as "everything is consistent".
	Unreadable int
}

// Reconcile is the backstop for the one thing the inbox cannot cover: a webhook
// that never arrived at all. It re-reads every subscription through the provider
// and applies the same CAS, then reports the inconsistencies it cannot fix.
func (s *Service) Reconcile(ctx context.Context, now time.Time) (ReconcileReport, error) {
	var report ReconcileReport
	if !s.payments.configured() {
		// Not an error: a deployment with no provider has nothing to reconcile
		// against, which is the self-hosted shape.
		slog.InfoContext(ctx, "no payments provider configured; nothing to reconcile")
		return report, nil
	}
	provider := s.payments.Provider

	// Paged by offset rather than keyset: the pass reads the whole table under no
	// lock, and a row inserted mid-walk being seen twice or not at all is
	// harmless -- the next pass covers it, and the CAS makes a repeat a no-op.
	for offset := int32(0); ; offset += reconcilePageSize {
		rows, err := s.read.ListBillingSubscriptionsByProvider(ctx,
			dbread.ListBillingSubscriptionsByProviderParams{
				Provider:  provider.Name(),
				RowLimit:  reconcilePageSize,
				RowOffset: offset,
			})
		if err != nil {
			slog.ErrorContext(ctx, "failed to list billing subscriptions", slogx.Error(err))
			telemetry.RecordError(ctx, err)
			return report, err
		}
		for _, row := range rows {
			s.reconcileOne(ctx, provider, row, now, &report)
		}
		if len(rows) < reconcilePageSize {
			break
		}
	}

	unbilled, err := s.read.ListPaidEntitlementsWithoutLiveSubscription(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list entitlements with no subscription", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return report, err
	}
	for _, row := range unbilled {
		report.EntitledUnbilled++
		// Loud on purpose: an org entitled to a paid plan that nobody is charged
		// for is the failure mode invariant 3 exists to make impossible.
		slog.ErrorContext(ctx, "org holds a paid entitlement with no live subscription",
			slog.String("org_id", row.OrgID), slog.String("plan_slug", row.PlanSlug))
	}

	slog.InfoContext(ctx, "billing reconcile pass finished",
		slog.Int("checked", report.Checked), slog.Int("applied", report.Applied),
		slog.Int("untracked", report.Untracked), slog.Int("entitled_unbilled", report.EntitledUnbilled),
		slog.Int("unmapped_product", report.UnmappedProduct), slog.Int("unreadable", report.Unreadable))
	return report, nil
}

// reconcileOne re-reads one subscription and applies it. Errors are counted and
// logged rather than returned: one unreadable subscription must not abandon the
// rest of the pass.
func (s *Service) reconcileOne(
	ctx context.Context, provider PaymentProvider, row dbread.BillingSubscription,
	now time.Time, report *ReconcileReport,
) {
	report.Checked++

	event, err := provider.FetchSubscription(ctx, row.ProviderSubID)
	if err != nil {
		report.Unreadable++
		slog.ErrorContext(ctx, "failed to re-read a subscription from the provider", slogx.Error(err),
			slog.String("org_id", row.OrgID), slog.String("provider_sub_id", row.ProviderSubID))
		telemetry.RecordError(ctx, err)
		return
	}
	if event.IsZero() {
		report.Untracked++
		slog.ErrorContext(ctx, "the provider returned nothing for a stored subscription",
			slog.String("org_id", row.OrgID), slog.String("provider_sub_id", row.ProviderSubID))
		return
	}

	rec, err := s.StoredRecord(ctx, row.OrgID)
	if err != nil {
		report.Unreadable++
		return
	}
	if _, err := s.planForProduct(event.ProductID, rec); err != nil {
		report.UnmappedProduct++
		slog.ErrorContext(ctx, "live subscription names a product pug cannot place", slogx.Error(err),
			slog.String("org_id", row.OrgID), slog.String("product_id", event.ProductID))
		telemetry.RecordError(ctx, err)
		return
	}

	// `now` rather than a delivery timestamp: a read is as fresh as the clock it
	// was made at, and the CAS then refuses it only if a webhook has landed
	// something newer in between -- which is the correct outcome.
	applied, err := s.applyReconciledSubscription(ctx, provider, row.OrgID, event, rec, now)
	if err != nil {
		report.Unreadable++
		return
	}
	if applied {
		report.Applied++
	}
}
