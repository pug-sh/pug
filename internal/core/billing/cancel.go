package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	coreusage "github.com/pug-sh/pug/internal/core/usage"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
)

// cancelMargin keeps a charge a day clear of its mandate's end, which absorbs the meter's
// hourly stamp and the pass's own hour.
const cancelMargin = 24 * time.Hour

var (
	ErrNoMandate = errors.New("billing: this org has no live payment method")
	// ErrFinalPeriodUnsettled leaves the mandate live: cancelling first would turn a
	// retryable decline into a write-off.
	ErrFinalPeriodUnsettled = errors.New("billing: a charge has not settled; the payment method is unchanged")
	// ErrRemoveIncomplete is a removal that failed after it began charging. The caller
	// must not be answered as though no money had moved.
	ErrRemoveIncomplete = errors.New("billing: the payment method was not removed")
)

// RemovePaymentMethod is pug's own cancellation, in the order the portal cannot promise:
// close and charge every day the meter has finalized, and refuse to cancel until every
// charge has settled.
func (s *Service) RemovePaymentMethod(ctx context.Context, orgID, actor string, now time.Time, grace time.Duration) error {
	if !s.billingEnabled || !s.payments.configured() {
		return ErrNoProvider
	}
	if strings.TrimSpace(actor) == "" {
		return ErrActorRequired
	}
	mandate, err := s.liveSubscription(ctx, orgID)
	if err != nil {
		return err
	}
	if mandate == nil || !mandate.OnDemand {
		return ErrNoMandate
	}
	orgCreate, err := orgCreateTime(ctx, s.write(), orgID)
	if err != nil {
		return err
	}

	var closed CloseReport
	if err := s.closeOrg(ctx, orgID, orgCreate, now, grace, now, actor, &closed); err != nil {
		return err
	}
	read := dbread.New(s.pgW)
	due, err := read.ListDueBillingInvoices(ctx, dbread.ListDueBillingInvoicesParams{
		Now: postgres.NewTimestamptz(now), OrgID: orgID,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to list the invoices a removal charges", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return err
	}
	var charged ChargeReport
	for _, inv := range due {
		if err := s.chargeOne(ctx, inv, now, actor, &charged); err != nil {
			return fmt.Errorf("%w: %w", ErrRemoveIncomplete, err)
		}
	}
	// Polled now rather than left to the pass: an accepted charge is not yet money.
	charges, err := read.ListBillingInvoicesToSettle(ctx, orgID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list the charges a removal waits on", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return err
	}
	var settled SettleReport
	for _, inv := range charges {
		if inv.Status != string(InvoiceCharged) {
			continue
		}
		if err := s.pollCharged(ctx, inv, now, actor, &settled); err != nil {
			return err
		}
	}
	// The ledger rather than the reports: a retry of this call may close and charge nothing.
	unsettled, err := read.HasUnsettledBillingInvoice(ctx, orgID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to read the invoices a removal waits on", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return err
	}
	if unsettled || closed.Held > 0 || closed.Unpriceable > 0 {
		slog.WarnContext(ctx, "not removing a payment method before its charges settle",
			slog.String("org_id", orgID), slog.Int("held", closed.Held), slog.Int("unpriceable", closed.Unpriceable))
		return ErrFinalPeriodUnsettled
	}

	provider := s.payments.Provider
	event, err := provider.CancelSubscription(ctx, mandate.ProviderSubID)
	if err == nil && event.Status.Live() {
		err = fmt.Errorf("billing: subscription %s still reads %s once cancelled", mandate.ProviderSubID, event.Status)
	}
	if err != nil {
		slog.ErrorContext(ctx, "failed to cancel a mandate", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("provider_sub_id", mandate.ProviderSubID))
		telemetry.RecordError(ctx, err)
		return fmt.Errorf("%w: %w", ErrRemoveIncomplete, err)
	}
	// The cancellation stands either way; the webhook and reconcile mirror it too.
	ctx, cancel := recording(ctx)
	defer cancel()
	if applied, err := s.applySubscription(ctx, provider, orgID, event, now, actor); err != nil || applied == 0 {
		// Until it is mirrored the org still reads chargeable, with a next charge date.
		slog.WarnContext(ctx, "a removed payment method is not mirrored yet", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("provider_sub_id", mandate.ProviderSubID))
		telemetry.RecordError(ctx, err)
	}
	return nil
}

// PinReport is what one pin step did and found.
type PinReport struct {
	Pinned int
	// Unreadable is a mandate whose date could not be moved, or the answer not stored.
	Unreadable int
}

// PinNextCharges moves the next billing date of each live mandate with no cancellation
// scheduled to a day after its current period's charge, which is where a cancellation
// from the provider's portal lands.
func (s *Service) PinNextCharges(ctx context.Context, now time.Time, grace time.Duration) (PinReport, error) {
	var r PinReport
	if !s.billingEnabled || !s.payments.configured() {
		return r, nil
	}
	provider := s.payments.Provider
	rows, err := dbread.New(s.pgW).ListPinnableBillingMandates(ctx, provider.Name())
	if err != nil {
		slog.ErrorContext(ctx, "failed to list the mandates to pin", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return r, err
	}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return r, err
		}
		_, end := coreusage.PeriodFor(now, coreusage.AnchorDay(row.OrgCreateTime.Time, postgres.Int2ToInt(row.AnchorDay)))
		want := chargeAfter(end, grace).Add(cancelMargin)
		// At day grain: the provider may keep a time of day of its own.
		wantDay := coreusage.FloorDayUTC(want)
		if coreusage.FloorDayUTC(row.CurrentPeriodEnd.Time).Equal(wantDay) {
			continue
		}
		event, err := provider.SetNextBillingDate(ctx, row.ProviderSubID, want)
		if err == nil && !coreusage.FloorDayUTC(event.CurrentPeriodEnd).Equal(wantDay) {
			err = fmt.Errorf("billing: subscription %s holds %s after a pin to %s", row.ProviderSubID,
				event.CurrentPeriodEnd.Format(time.DateOnly), wantDay.Format(time.DateOnly))
		}
		if err != nil {
			r.Unreadable++
			slog.ErrorContext(ctx, "failed to pin a mandate's next billing date", slogx.Error(err),
				slog.String("org_id", row.OrgID), slog.String("provider_sub_id", row.ProviderSubID))
			telemetry.RecordError(ctx, err)
			continue
		}
		// Logged at the write.
		if _, err := s.applySubscription(ctx, provider, row.OrgID, event, now, ActorInvoicePass); err != nil {
			r.Unreadable++
			continue
		}
		r.Pinned++
	}
	return r, nil
}

// scheduledCutoff is the last instant a mandate scheduled to cancel can be charged: zero
// while none is, or while its end is unread.
func scheduledCutoff(subs []Subscription) time.Time {
	for _, sub := range subs {
		if sub.OnDemand && sub.Status.Live() && sub.CancelAtPeriodEnd && !sub.CurrentPeriodEnd.IsZero() {
			return sub.CurrentPeriodEnd.Add(-cancelMargin)
		}
	}
	return time.Time{}
}

// chargeBy brings the org's open invoices' charges forward to by, when their notice
// windows run past it.
func (s *Service) chargeBy(ctx context.Context, orgID string, by time.Time) error {
	if _, err := s.write().ChargeBillingInvoicesBy(ctx, dbwrite.ChargeBillingInvoicesByParams{
		ChargeBy: postgres.NewTimestamptz(by), OrgID: orgID,
	}); err != nil {
		slog.ErrorContext(ctx, "failed to bring an invoice's charge forward", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return err
	}
	return nil
}
