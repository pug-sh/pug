package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/slogx"
)

// reconcilePageSize bounds one read of the subscription table. The pass walks
// every row, so this is a memory bound, not a limit on what it covers.
const reconcilePageSize = 500

// DeliveryRetention is how long a processed delivery is kept. Its payload holds
// personal data pug does not otherwise store, and only replay needs the bytes.
const DeliveryRetention = 90 * 24 * time.Hour

// deliveryStaleAfter is how long a delivery may sit unprocessed before the pass
// calls it stranded. The provider's retries are spent within minutes, so a day
// is well past "still in flight".
const deliveryStaleAfter = 24 * time.Hour

// ReconcileReport is what one pass found. Nothing is auto-fixed: that would be
// writing to the money side from a guess. The counts are for alerting.
type ReconcileReport struct {
	// Rows re-read from the provider and re-applied through the same CAS the
	// webhook uses.
	Checked int
	Applied int

	// A subscription pug stores that the provider no longer knows. A finding for a
	// person: nothing here tells a purged subscription from one never theirs.
	Untracked int
	// A deal in force with no live mandate behind it.
	EntitledUnbilled int
	// Rows the pass could not settle: a failed read or write, or a read that
	// decoded to nothing. The only counter the CronJob fails on.
	Unreadable int
	// A live subscription pug cannot apply: an unsold currency, no status, no
	// customer, a negative price, or not on-demand. Counted rather than skipped, or
	// the pass reports a sweep it did not make.
	Unapplicable int
	// Two live subscriptions for one org, refused by the partial unique index — the
	// one finding that means an org may be paying twice.
	TwoLive int
	// Deliveries accepted and not applied. The walk above cannot see them: one that
	// was not applied wrote no subscription row.
	Rejected int
	// Deliveries that never settled: every retry failed, so nothing recorded a
	// reason. The one outcome no other counter can represent.
	Stranded int
}

// Reconcile is the backstop for the one thing the inbox cannot cover: a webhook
// that never arrived. It re-reads every subscription and applies the same CAS.
func (s *Service) Reconcile(ctx context.Context, now time.Time) (ReconcileReport, error) {
	var report ReconcileReport
	if !s.payments.configured() {
		// Not an error: a deployment with no provider has nothing to reconcile
		// against, which is the self-hosted shape.
		slog.InfoContext(ctx, "no payments provider configured; nothing to reconcile")
		return report, nil
	}
	provider := s.payments.Provider

	// Paged by offset rather than keyset: the walk holds no lock, and a row seen
	// twice or not at all is harmless — the CAS makes a repeat a no-op.
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
			// Without this the pass turns one cancelled context into a per-row provider
			// error, reporting an outage that never happened.
			if err := ctx.Err(); err != nil {
				slog.ErrorContext(ctx, "billing reconcile pass was cut short", slogx.Error(err),
					slog.Int("checked", report.Checked))
				telemetry.RecordError(ctx, err)
				return report, err
			}
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
		slog.WarnContext(ctx, "org holds a deal with no live mandate",
			slog.String("org_id", row.OrgID), slog.String("plan_slug", row.PlanSlug))
	}

	// The whole retention window, not since the last pass: a rejection is a person's
	// to act on, so it is re-reported every run until the payload ages out.
	rejected, err := s.read.ListRecentRejectedBillingWebhookDeliveries(ctx,
		postgres.NewTimestamptz(now.Add(-DeliveryRetention)))
	if err != nil {
		slog.ErrorContext(ctx, "failed to list rejected billing webhook deliveries", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return report, err
	}
	for _, row := range rejected {
		report.Rejected++
		// Error, not warn: somebody may have paid for a plan they do not hold.
		slog.ErrorContext(ctx, "billing webhook delivery was accepted but not applied",
			slog.String("provider", row.Provider), slog.String("webhook_id", row.WebhookID),
			slog.String("event_type", row.EventType), slog.String("reason", row.Error))
	}

	stranded, err := s.read.ListStrandedBillingWebhookDeliveries(ctx,
		postgres.NewTimestamptz(now.Add(-deliveryStaleAfter)))
	if err != nil {
		slog.ErrorContext(ctx, "failed to list stranded billing webhook deliveries", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return report, err
	}
	for _, row := range stranded {
		report.Stranded++
		// The delivery is spent: the provider will not send it again, and no reason
		// was ever recorded. A person has to read the stored payload.
		slog.ErrorContext(ctx, "billing webhook delivery was never settled",
			slog.String("provider", row.Provider), slog.String("webhook_id", row.WebhookID),
			slog.String("event_type", row.EventType))
	}

	slog.InfoContext(ctx, "billing reconcile pass finished",
		slog.Int("checked", report.Checked), slog.Int("applied", report.Applied),
		slog.Int("untracked", report.Untracked), slog.Int("entitled_unbilled", report.EntitledUnbilled),
		slog.Int("unreadable", report.Unreadable),
		slog.Int("two_live", report.TwoLive), slog.Int("rejected", report.Rejected),
		slog.Int("stranded", report.Stranded), slog.Int("unapplicable", report.Unapplicable))
	return report, nil
}

// reconcileOne re-reads one subscription and applies it. Errors are counted and
// logged, not returned: one unreadable row must not abandon the pass.
func (s *Service) reconcileOne(
	ctx context.Context, provider PaymentProvider, row dbread.BillingSubscription,
	now time.Time, report *ReconcileReport,
) {
	report.Checked++

	event, err := provider.FetchSubscription(ctx, row.ProviderSubID)
	if err != nil {
		// A subscription the provider does not know is a finding, not an outage:
		// counting it unreadable would hold the CronJob red on every later run.
		if errors.Is(err, ErrSubscriptionNotFound) {
			report.Untracked++
			slog.ErrorContext(ctx, "the provider does not know a stored subscription", slogx.Error(err),
				slog.String("org_id", row.OrgID), slog.String("provider_sub_id", row.ProviderSubID))
			return
		}
		report.Unreadable++
		slog.ErrorContext(ctx, "failed to re-read a subscription from the provider", slogx.Error(err),
			slog.String("org_id", row.OrgID), slog.String("provider_sub_id", row.ProviderSubID))
		telemetry.RecordError(ctx, err)
		return
	}
	if event.IsZero() {
		// A successful read that decodes to nothing is a shape change, not a dropped
		// subscription: Untracked would exit 0 on a pass that verified nothing.
		report.Unreadable++
		err := fmt.Errorf("provider returned an undecodable subscription %q", row.ProviderSubID)
		slog.ErrorContext(ctx, "the provider returned nothing for a stored subscription", slogx.Error(err),
			slog.String("org_id", row.OrgID), slog.String("provider_sub_id", row.ProviderSubID))
		telemetry.RecordError(ctx, err)
		return
	}

	// Only to tell an org that has vanished from an ordinary read failure: the
	// writer re-reads the row itself, under the lock it applies in.
	if _, err := s.StoredRecord(ctx, row.OrgID); err != nil {
		report.Unreadable++
		// StoredRecord logs a real read failure; an org that has vanished from under
		// a live subscription is the one it returns silently.
		if errors.Is(err, ErrOrgNotFound) {
			slog.ErrorContext(ctx, "live subscription names an org that is gone",
				slog.String("org_id", row.OrgID), slog.String("provider_sub_id", row.ProviderSubID))
		}
		return
	}
	// `now` rather than a delivery timestamp: a read is as fresh as the clock it was
	// made at, and the CAS then refuses it only if a webhook landed something newer.
	applied, err := s.applySubscription(ctx, provider, row.OrgID, event, now)
	if err != nil {
		// Already logged at the write, except the two states the writer names rather
		// than fails on — and every one of them has to land on a counter, or the pass
		// reports a sweep it did not make.
		switch {
		case errors.Is(err, ErrTwoLiveSubscriptions):
			// An inconsistency rather than a failed read, but it means an org may be
			// billed twice.
			report.TwoLive++
		case errors.Is(err, ErrSubscriptionUnapplicable):
			report.Unapplicable++
			slog.ErrorContext(ctx, "live subscription cannot be applied", slogx.Error(err), // puglint:exempt — recorded by applySubscription
				slog.String("org_id", row.OrgID), slog.String("provider_sub_id", row.ProviderSubID),
				slog.String("currency", normalizeCurrency(event.Currency)),
				slog.String("status", string(event.Status)), slog.Bool("on_demand", event.OnDemand))
		default:
			report.Unreadable++
		}
		return
	}
	if applied > 0 {
		report.Applied++
	}
}
