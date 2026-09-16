package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
)

const codeMandateGone = "mandate_gone"

// ChargeReport is what one charge step did and found.
type ChargeReport struct {
	Charged int
	// Declined is a charge the card refused, left failed.
	Declined int
	// Ambiguous is a charge left charging, to be settled by reading.
	Ambiguous int
	// MandateGone is an invoice written off because the org's mandate has ended.
	MandateGone int
	// AwaitingCard is a deal's invoice held for an org that never had a mandate (§19.15).
	AwaitingCard int
	// Unreadable is a provider read that failed, a second 404 included.
	Unreadable int
}

// ChargeDue charges every open or failed invoice whose charge date has come. A
// Postgres failure stops only its own invoice, and the first is returned.
func (s *Service) ChargeDue(ctx context.Context, now time.Time) (ChargeReport, error) {
	var r ChargeReport
	if !s.billingEnabled || !s.payments.configured() {
		return r, nil
	}
	due, err := dbread.New(s.pgW).ListDueBillingInvoices(ctx, postgres.NewTimestamptz(now))
	if err != nil {
		slog.ErrorContext(ctx, "failed to list the invoices due a charge", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return r, err
	}
	var firstErr error
	for _, inv := range due {
		if err := ctx.Err(); err != nil {
			return r, err
		}
		if err := s.chargeOne(ctx, inv, now, &r); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return r, firstErr
}

func (s *Service) chargeOne(ctx context.Context, inv dbread.ListDueBillingInvoicesRow, now time.Time, r *ChargeReport) error {
	subs, err := s.subscriptionsOf(ctx, inv.OrgID)
	if err != nil {
		return err
	}
	live := slices.IndexFunc(subs, func(sub Subscription) bool { return sub.OnDemand && sub.Status.Live() })
	if live < 0 {
		// A deal recorded before its first card waits for one (§19.15).
		if !slices.ContainsFunc(subs, func(sub Subscription) bool { return sub.OnDemand }) {
			r.AwaitingCard++
			return nil
		}
		return s.writeOff(ctx, inv, "no live mandate to charge", now, r)
	}
	mandate := subs[live]
	provider := s.payments.Provider

	// Committed before the provider is called, so a process dying in between leaves a
	// row that says so.
	claimed, err := s.moveInvoice(ctx, inv.OrgID, inv.ID, "mandate "+mandate.ProviderSubID,
		func(w *dbwrite.Queries) (dbwrite.BillingInvoice, error) {
			return w.MarkBillingInvoiceCharging(ctx, dbwrite.MarkBillingInvoiceChargingParams{
				ID:            inv.ID,
				Provider:      postgres.NewOptionalText(provider.Name()),
				ProviderSubID: postgres.NewOptionalText(mandate.ProviderSubID),
			})
		})
	if err != nil || !claimed {
		return err
	}

	paymentID, chargeErr := provider.Charge(ctx, ChargeInput{
		AmountCents:   inv.AmountCents,
		Currency:      inv.Currency,
		Description:   chargeDescription(inv),
		InvoiceID:     inv.ID,
		OrgID:         inv.OrgID,
		PeriodStart:   inv.PeriodStart.Time,
		ProviderSubID: mandate.ProviderSubID,
	})
	var refused *ChargeError
	switch {
	case chargeErr == nil:
		r.Charged++
		_, err = s.moveInvoice(ctx, inv.OrgID, inv.ID, "payment "+paymentID,
			func(w *dbwrite.Queries) (dbwrite.BillingInvoice, error) {
				return w.MarkBillingInvoiceCharged(ctx, dbwrite.MarkBillingInvoiceChargedParams{
					ID: inv.ID, ProviderPaymentID: postgres.NewOptionalText(paymentID),
				})
			})
		return err
	case !errors.As(chargeErr, &refused):
		r.Ambiguous++
		return s.leaveCharging(ctx, inv, "", chargeErr.Error(), chargeErr)
	case refused.Declined:
		r.Declined++
		slog.WarnContext(ctx, "a charge was declined", slog.String("org_id", inv.OrgID),
			slog.String("invoice_id", inv.ID), slog.String("code", refused.Code))
		_, err = s.moveInvoice(ctx, inv.OrgID, inv.ID, "declined "+refused.Code,
			func(w *dbwrite.Queries) (dbwrite.BillingInvoice, error) {
				return w.MarkBillingInvoiceFailed(ctx, dbwrite.MarkBillingInvoiceFailedParams{
					FailedAt:         postgres.NewTimestamptz(now),
					ID:               inv.ID,
					LastErrorCode:    refused.Code,
					LastErrorMessage: refused.Message,
				})
			})
		return err
	case refused.NotChargeable:
		return s.corroborate(ctx, provider, inv, mandate, refused, now, r)
	default:
		r.Ambiguous++
		return s.leaveCharging(ctx, inv, refused.Code, refused.Message, refused)
	}
}

// corroborate writes an invoice off on a not-chargeable refusal only once the mandate
// also reads back ended. A second 404 proves nothing: a wrong environment answers both.
func (s *Service) corroborate(
	ctx context.Context, provider PaymentProvider, inv dbread.ListDueBillingInvoicesRow,
	mandate Subscription, refused *ChargeError, now time.Time, r *ChargeReport,
) error {
	event, err := provider.FetchSubscription(ctx, mandate.ProviderSubID)
	if err == nil && event.IsZero() {
		err = fmt.Errorf("billing: subscription %s reads back as nothing", mandate.ProviderSubID)
	}
	if err != nil {
		r.Unreadable++
		return s.leaveCharging(ctx, inv, refused.Code, refused.Message, err)
	}
	if status, ok := ParseSubStatus(string(event.Status)); !ok || status.Live() {
		r.Ambiguous++
		return s.leaveCharging(ctx, inv, refused.Code, refused.Message,
			fmt.Errorf("%w, but the mandate reads back %q", refused, event.Status))
	}
	return s.writeOff(ctx, inv, refused.Message, now, r)
}

// leaveCharging records a charge to be settled by reading, never by charging again.
func (s *Service) leaveCharging(
	ctx context.Context, inv dbread.ListDueBillingInvoicesRow, code, message string, cause error,
) error {
	slog.ErrorContext(ctx, "a charge is left unsettled", slogx.Error(cause),
		slog.String("org_id", inv.OrgID), slog.String("invoice_id", inv.ID))
	telemetry.RecordError(ctx, cause)
	if _, err := s.write().RecordBillingInvoiceChargeError(ctx, dbwrite.RecordBillingInvoiceChargeErrorParams{
		ID: inv.ID, LastErrorCode: code, LastErrorMessage: message,
	}); err != nil {
		slog.ErrorContext(ctx, "failed to record a charge error", slogx.Error(err),
			slog.String("org_id", inv.OrgID), slog.String("invoice_id", inv.ID))
		telemetry.RecordError(ctx, err)
		return err
	}
	return nil
}

func (s *Service) writeOff(
	ctx context.Context, inv dbread.ListDueBillingInvoicesRow, message string, now time.Time, r *ChargeReport,
) error {
	moved, err := s.moveInvoice(ctx, inv.OrgID, inv.ID, codeMandateGone,
		func(w *dbwrite.Queries) (dbwrite.BillingInvoice, error) {
			return w.MarkBillingInvoiceUncollectible(ctx, dbwrite.MarkBillingInvoiceUncollectibleParams{
				FailedAt:         postgres.NewTimestamptz(now),
				ID:               inv.ID,
				LastErrorCode:    codeMandateGone,
				LastErrorMessage: message,
			})
		})
	if err != nil || !moved {
		return err
	}
	r.MandateGone++
	slog.WarnContext(ctx, "an invoice was written off because its mandate has ended",
		slog.String("org_id", inv.OrgID), slog.String("invoice_id", inv.ID))
	return nil
}

// moveInvoice applies one guarded update under the row's lock and records it. false
// is an invoice another writer moved first.
func (s *Service) moveInvoice(
	ctx context.Context, orgID, invoiceID, detail string,
	update func(*dbwrite.Queries) (dbwrite.BillingInvoice, error),
) (bool, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	w := dbwrite.New(tx)
	from, err := w.GetBillingInvoiceStatusForUpdate(ctx, invoiceID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to lock an invoice", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("invoice_id", invoiceID))
		telemetry.RecordError(ctx, err)
		return false, err
	}
	row, err := update(w)
	if errors.Is(err, pgx.ErrNoRows) {
		slog.WarnContext(ctx, "an invoice moved before this write landed", slog.String("org_id", orgID),
			slog.String("invoice_id", invoiceID), slog.String("status", from))
		return false, nil
	}
	if err != nil {
		slog.ErrorContext(ctx, "failed to move an invoice", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("invoice_id", invoiceID))
		telemetry.RecordError(ctx, err)
		return false, err
	}
	if err := appendInvoiceEvent(ctx, w, orgID, invoiceID, InvoiceStatus(from), InvoiceStatus(row.Status), detail); err != nil {
		return false, err
	}
	if err := s.commit(ctx, tx, orgID); err != nil {
		return false, err
	}
	return true, nil
}

func chargeDescription(inv dbread.ListDueBillingInvoicesRow) string {
	lastDay := inv.BilledTo.Time.AddDate(0, 0, -1)
	desc := fmt.Sprintf("Pug: %s events, %s to %s",
		comma(inv.EventCount), inv.BilledFrom.Time.Format("2 Jan"), lastDay.Format("2 Jan 2006"))
	if inv.CarriedCents == 0 {
		return desc
	}
	periods := "periods"
	if inv.CarriedPeriods == 1 {
		periods = "period"
	}
	return desc + fmt.Sprintf(", and $%d.%02d carried from %d earlier %s",
		inv.CarriedCents/100, inv.CarriedCents%100, inv.CarriedPeriods, periods)
}
