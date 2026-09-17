package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
)

const (
	// settleChargingAfter gives a payment the provider just created time to list.
	settleChargingAfter = 5 * time.Minute
	// pollChargedAfter leaves an accepted charge to its webhook first.
	pollChargedAfter = time.Hour
	// providerClockSkew widens a listing back from the charging instant, which is pug's clock.
	providerClockSkew = time.Minute
)

// SettleReport is what one settle step did and found.
type SettleReport struct {
	Paid int
	// Adopted is a charge whose payment a read found still in flight, now charged.
	Adopted  int
	Failed   int
	Reopened int
	// Pending is a charged payment still in flight, asked about again next pass.
	Pending int
	// Duplicate is a payment that took money with no bill behind it: a refund by hand.
	Duplicate      int
	AmountMismatch int
	// Unreadable is a payment pug could not read, or a charge another provider made.
	Unreadable int
}

// SettleCharges resolves by reading what a charge's answer could not: a charging
// invoice off its mandate's payments, a charged one off its own payment. A Postgres
// failure stops only its own invoice, and the first is returned.
func (s *Service) SettleCharges(ctx context.Context, now time.Time) (SettleReport, error) {
	var r SettleReport
	if !s.billingEnabled {
		return r, nil
	}
	rows, err := dbread.New(s.pgW).ListBillingInvoicesToSettle(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list the invoices to settle", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return r, err
	}
	due := slices.DeleteFunc(rows, func(inv dbread.ListBillingInvoicesToSettleRow) bool {
		wait := pollChargedAfter
		if inv.Status == string(InvoiceCharging) {
			wait = settleChargingAfter
		}
		return inv.EnteredAt.Time.After(now.Add(-wait))
	})
	if len(due) > 0 && !s.payments.configured() {
		err := fmt.Errorf("%w: %d charges are unsettled", ErrNoProvider, len(due))
		slog.ErrorContext(ctx, "charges are unsettled and nothing can read them", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return r, err
	}
	var firstErr error
	for _, inv := range due {
		if err := ctx.Err(); err != nil {
			return r, err
		}
		if name := s.payments.Provider.Name(); inv.Provider.String != name {
			r.Unreadable++
			err := fmt.Errorf("billing: invoice %s was charged through %q, not %q", inv.ID, inv.Provider.String, name)
			slog.ErrorContext(ctx, "a charge cannot be settled by this provider", slogx.Error(err),
				slog.String("org_id", inv.OrgID), slog.String("invoice_id", inv.ID))
			telemetry.RecordError(ctx, err)
			continue
		}
		var err error
		if inv.Status == string(InvoiceCharging) {
			err = s.settleCharging(ctx, inv, now, &r)
		} else {
			err = s.pollCharged(ctx, inv, now, &r)
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return r, firstErr
}

// settleCharging reads a charge's outcome off its mandate's payments, listed from the
// charging instant: an earlier attempt's payment carries the same invoice id.
func (s *Service) settleCharging(ctx context.Context, inv dbread.ListBillingInvoicesToSettleRow, now time.Time, r *SettleReport) error {
	since := inv.EnteredAt.Time.Add(-providerClockSkew)
	listed, err := s.payments.Provider.ListPayments(ctx, inv.ProviderSubID.String, since)
	if err != nil {
		r.Unreadable++
		slog.ErrorContext(ctx, "failed to list the payments that settle a charge", slogx.Error(err),
			slog.String("org_id", inv.OrgID), slog.String("invoice_id", inv.ID))
		telemetry.RecordError(ctx, err)
		return nil
	}
	var live, failed []Payment
	for _, p := range listed {
		switch {
		case p.InvoiceID != inv.ID || !p.CreatedAt.IsZero() && p.CreatedAt.Before(since):
		case p.Status == PaymentFailed:
			failed = append(failed, p)
		default:
			live = append(live, p)
		}
	}
	// The list promises no order, and a retry shares the invoice id with the attempt
	// that failed before it.
	newestFirst := func(a, b Payment) int { return b.CreatedAt.Compare(a.CreatedAt) }
	slices.SortStableFunc(live, newestFirst)
	slices.SortStableFunc(failed, newestFirst)
	var pick Payment
	switch {
	case len(live) > 0:
		pick = live[0]
	case len(failed) > 0:
		pick = failed[0]
	default:
		moved, err := s.moveInvoice(ctx, inv.OrgID, inv.ID, ActorInvoicePass, "no payment found",
			func(w *dbwrite.Queries) (dbwrite.BillingInvoice, error) {
				return w.ReopenBillingInvoiceCharge(ctx, dbwrite.ReopenBillingInvoiceChargeParams{
					ID: inv.ID, NextAttemptAt: postgres.NewTimestamptz(now),
				})
			})
		if moved {
			r.Reopened++
		}
		return err
	}
	payment, ok := s.fetchPayment(ctx, inv, pick.PaymentID, r)
	if !ok {
		return nil
	}
	if len(live) > 1 {
		for _, dup := range live[1:] {
			if err := appendInvoiceEvent(ctx, s.write(), inv.OrgID, inv.ID, ActorInvoicePass,
				InvoiceCharging, InvoiceCharging, "duplicate payment "+dup.PaymentID); err != nil {
				return err
			}
			r.Duplicate++
			reportDuplicate(ctx, inv.OrgID, inv.ID, dup.PaymentID)
		}
	}

	switch payment.Status {
	case PaymentSucceeded:
		return s.settlePaid(ctx, inv.OrgID, inv.ID, payment, now, ActorInvoicePass, r)
	case PaymentFailed:
		return s.settleFailed(ctx, inv.OrgID, inv.ID, InvoiceCharging, payment, now, ActorInvoicePass, r)
	case PaymentProcessing:
	}
	moved, err := s.moveInvoice(ctx, inv.OrgID, inv.ID, ActorInvoicePass, "payment "+payment.PaymentID,
		func(w *dbwrite.Queries) (dbwrite.BillingInvoice, error) {
			return w.MarkBillingInvoiceCharged(ctx, dbwrite.MarkBillingInvoiceChargedParams{
				ID: inv.ID, ProviderPaymentID: postgres.NewOptionalText(payment.PaymentID),
			})
		})
	if moved {
		r.Adopted++
	}
	return err
}

// pollCharged asks what became of a charge the provider accepted, for a deployment
// its webhook never reaches.
func (s *Service) pollCharged(ctx context.Context, inv dbread.ListBillingInvoicesToSettleRow, now time.Time, r *SettleReport) error {
	payment, ok := s.fetchPayment(ctx, inv, inv.ProviderPaymentID.String, r)
	if !ok {
		return nil
	}
	switch payment.Status {
	case PaymentSucceeded:
		return s.settlePaid(ctx, inv.OrgID, inv.ID, payment, now, ActorInvoicePass, r)
	case PaymentFailed:
		return s.settleFailed(ctx, inv.OrgID, inv.ID, InvoiceCharged, payment, now, ActorInvoicePass, r)
	case PaymentProcessing:
	}
	r.Pending++
	return nil
}

func (s *Service) fetchPayment(
	ctx context.Context, inv dbread.ListBillingInvoicesToSettleRow, paymentID string, r *SettleReport,
) (Payment, bool) {
	payment, err := s.payments.Provider.FetchPayment(ctx, paymentID)
	if err == nil && payment.PaymentID != paymentID {
		err = fmt.Errorf("billing: payment %s reads back as %q", paymentID, payment.PaymentID)
	}
	if err != nil {
		r.Unreadable++
		slog.ErrorContext(ctx, "failed to read the payment that settles a charge", slogx.Error(err),
			slog.String("org_id", inv.OrgID), slog.String("invoice_id", inv.ID), slog.String("payment_id", paymentID))
		telemetry.RecordError(ctx, err)
		return Payment{}, false
	}
	return payment, true
}

func (s *Service) settleFailed(
	ctx context.Context, orgID, invoiceID string, from InvoiceStatus, p Payment, now time.Time, actor string,
	r *SettleReport,
) error {
	moved, err := s.moveInvoice(ctx, orgID, invoiceID, actor, strings.TrimSpace("declined "+p.ErrorCode),
		func(w *dbwrite.Queries) (dbwrite.BillingInvoice, error) {
			return w.MarkBillingInvoicePaymentFailed(ctx, dbwrite.MarkBillingInvoicePaymentFailedParams{
				FailedAt:          postgres.NewTimestamptz(now),
				FromStatus:        string(from),
				ID:                invoiceID,
				LastErrorCode:     p.ErrorCode,
				LastErrorMessage:  p.ErrorMessage,
				ProviderPaymentID: postgres.NewOptionalText(p.PaymentID),
			})
		})
	if moved {
		r.Failed++
	}
	return err
}

// settlePaid marks an invoice paid with the deferred rows it carries. A success that
// cannot land took money with no bill behind it, and is recorded rather than dropped.
func (s *Service) settlePaid(
	ctx context.Context, orgID, invoiceID string, p Payment, now time.Time, actor string, r *SettleReport,
) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	w := dbwrite.New(tx)
	cur, err := w.GetBillingInvoicePaymentForUpdate(ctx, invoiceID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to lock an invoice", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("invoice_id", invoiceID))
		telemetry.RecordError(ctx, err)
		return err
	}
	from := InvoiceStatus(cur.Status)
	_, err = w.MarkBillingInvoicePaid(ctx, dbwrite.MarkBillingInvoicePaidParams{
		ID:                 invoiceID,
		PaidAt:             postgres.NewTimestamptz(now),
		ProviderInvoiceUrl: postgres.NewOptionalText(p.InvoiceURL),
		ProviderPaymentID:  postgres.NewOptionalText(p.PaymentID),
		TaxCents:           pgtype.Int8{Int64: p.TaxCents, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// A webhook and the poll can both see the one payment.
		if cur.ProviderPaymentID.String == p.PaymentID {
			return nil
		}
		if err := appendInvoiceEvent(ctx, w, orgID, invoiceID, actor, from, from, "duplicate payment "+p.PaymentID); err != nil {
			return err
		}
		if err := s.commit(ctx, tx, orgID); err != nil {
			return err
		}
		r.Duplicate++
		reportDuplicate(ctx, orgID, invoiceID, p.PaymentID)
		return nil
	}
	if err != nil {
		slog.ErrorContext(ctx, "failed to mark an invoice paid", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("invoice_id", invoiceID))
		telemetry.RecordError(ctx, err)
		return err
	}
	if err := appendInvoiceEvent(ctx, w, orgID, invoiceID, actor, from, InvoicePaid, "payment "+p.PaymentID); err != nil {
		return err
	}
	covered, err := w.PayCoveredBillingInvoices(ctx, dbwrite.PayCoveredBillingInvoicesParams{
		CoveredBy: postgres.NewOptionalText(invoiceID), PaidAt: postgres.NewTimestamptz(now),
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to pay the invoices a payment covered", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("invoice_id", invoiceID))
		telemetry.RecordError(ctx, err)
		return err
	}
	for _, id := range covered {
		if err := appendInvoiceEvent(ctx, w, orgID, id, actor, InvoiceDeferred, InvoicePaid, "covered by "+invoiceID); err != nil {
			return err
		}
	}
	// total_amount includes the tax added on top, and pug billed the pre-tax figure.
	took := p.TotalCents - p.TaxCents
	mismatch := took != cur.AmountCents || normalizeCurrency(p.Currency) != cur.Currency
	if mismatch {
		detail := fmt.Sprintf("amount_mismatch: payment %s took %s %s before tax, billed %s %s",
			p.PaymentID, usd(took), normalizeCurrency(p.Currency), usd(cur.AmountCents), cur.Currency)
		if err := appendInvoiceEvent(ctx, w, orgID, invoiceID, actor, InvoicePaid, InvoicePaid, detail); err != nil {
			return err
		}
	}
	if err := s.commit(ctx, tx, orgID); err != nil {
		return err
	}
	r.Paid++
	if mismatch {
		r.AmountMismatch++
		err := fmt.Errorf("billing: invoice %s billed %d %s, and payment %s took %d %s before tax",
			invoiceID, cur.AmountCents, cur.Currency, p.PaymentID, took, normalizeCurrency(p.Currency))
		slog.ErrorContext(ctx, "a payment took a different amount from the one billed", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("invoice_id", invoiceID))
		telemetry.RecordError(ctx, err)
	}
	return nil
}

func reportDuplicate(ctx context.Context, orgID, invoiceID, paymentID string) {
	err := fmt.Errorf("billing: payment %s took money for invoice %s with no bill behind it", paymentID, invoiceID)
	slog.ErrorContext(ctx, "a payment needs refunding by hand", slogx.Error(err),
		slog.String("org_id", orgID), slog.String("invoice_id", invoiceID), slog.String("payment_id", paymentID))
	telemetry.RecordError(ctx, err)
}

func usd(cents int64) string { return fmt.Sprintf("$%d.%02d", cents/100, cents%100) }
