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

const (
	codeMandateGone = "mandate_gone"
	recordTimeout   = 30 * time.Second
)

// ChargeReport is what one charge step did and found.
type ChargeReport struct {
	Charged int
	// Declined is a charge the card refused, left failed with its retry dated.
	Declined int
	// Uncollectible is a refusal that is final: a hard decline, or the last attempt.
	Uncollectible int
	// Ambiguous is a charge left charging, to be settled by reading.
	Ambiguous int
	// MandateGone is an invoice written off because the org's mandate has ended.
	MandateGone int
	// AwaitingCard is a deal's invoice, or one whose org has no mandate on record, held
	// while none is live (§19.15). It is counted again on every pass it waits.
	AwaitingCard int
	// MandatePaused is an invoice held while its mandate is paused, which can resume.
	MandatePaused int
	// Unreadable is a mandate pug could not read: a failed provider read, a second 404,
	// or a stored status it has no word for.
	Unreadable int
}

// ChargeDue charges every open or failed invoice whose charge date has come. A
// Postgres failure stops only its own invoice, and the first is returned. Invoices
// due with no provider to charge them are an error, not a quiet pass.
func (s *Service) ChargeDue(ctx context.Context, now time.Time) (ChargeReport, error) {
	var r ChargeReport
	if !s.billingEnabled {
		return r, nil
	}
	due, err := dbread.New(s.pgW).ListDueBillingInvoices(ctx, dbread.ListDueBillingInvoicesParams{
		Now: postgres.NewTimestamptz(now),
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to list the invoices due a charge", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return r, err
	}
	if len(due) > 0 && !s.payments.configured() {
		err := fmt.Errorf("%w: %d invoices are due a charge", ErrNoProvider, len(due))
		slog.ErrorContext(ctx, "invoices are due and nothing can charge them", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return r, err
	}
	var firstErr error
	for _, inv := range due {
		if err := ctx.Err(); err != nil {
			return r, err
		}
		if err := s.chargeOne(ctx, inv, now, ActorInvoicePass, &r); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return r, firstErr
}

func (s *Service) chargeOne(
	ctx context.Context, inv dbread.ListDueBillingInvoicesRow, now time.Time, actor string, r *ChargeReport,
) error {
	subs, unnamed, err := s.subscriptionsOf(ctx, inv.OrgID)
	if err != nil {
		return err
	}
	live := slices.IndexFunc(subs, func(sub Subscription) bool { return sub.OnDemand && sub.Status.Live() })
	if live < 0 {
		return s.holdOrWriteOff(ctx, inv, subs, unnamed, now, actor, r)
	}
	mandate := subs[live]
	provider := s.payments.Provider

	// Committed before the provider is called, so a process dying in between leaves a
	// row that says so.
	claimed, err := s.moveInvoice(ctx, inv.OrgID, inv.ID, actor, "mandate "+mandate.ProviderSubID,
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
	if chargeErr == nil && paymentID == "" {
		chargeErr = errors.New("billing: the provider accepted a charge and returned no payment id")
	}

	var refused *ChargeError
	switch {
	case chargeErr == nil:
		ctx, cancel := recording(ctx)
		defer cancel()
		moved, err := s.moveInvoice(ctx, inv.OrgID, inv.ID, actor, "payment "+paymentID,
			func(w *dbwrite.Queries) (dbwrite.BillingInvoice, error) {
				return w.MarkBillingInvoiceCharged(ctx, dbwrite.MarkBillingInvoiceChargedParams{
					ID: inv.ID, ProviderPaymentID: postgres.NewOptionalText(paymentID),
				})
			})
		if err == nil && !moved {
			// The payment's own webhook can settle the invoice before this answer arrives.
			cur, readErr := s.write().GetBillingInvoice(ctx, inv.ID)
			switch {
			case readErr != nil:
				err = readErr
			case cur.Status == string(InvoicePaid) && cur.ProviderPaymentID.String == paymentID:
				r.Charged++
				return nil
			default:
				err = fmt.Errorf("billing: invoice %s moved before its payment was recorded", inv.ID)
			}
			telemetry.RecordError(ctx, err)
		}
		if err != nil {
			slog.ErrorContext(ctx, "a charge was taken but not recorded", slogx.Error(err), // puglint:exempt — recorded where it failed
				slog.String("org_id", inv.OrgID), slog.String("invoice_id", inv.ID), slog.String("payment_id", paymentID))
			return err
		}
		r.Charged++
		return nil
	case !errors.As(chargeErr, &refused):
		r.Ambiguous++
		return s.leaveCharging(ctx, inv, "", chargeErr.Error(), chargeErr)
	case refused.Declined:
		slog.WarnContext(ctx, "a charge was declined", slog.String("org_id", inv.OrgID),
			slog.String("invoice_id", inv.ID), slog.String("code", refused.Code))
		ctx, cancel := recording(ctx)
		defer cancel()
		moved, final, err := s.decline(ctx, inv.OrgID, inv.ID, InvoiceCharging,
			Payment{ErrorCode: refused.Code, ErrorMessage: refused.Message}, now, actor)
		switch {
		case !moved:
		case final:
			r.Uncollectible++
		default:
			r.Declined++
		}
		return err
	case refused.NotChargeable:
		return s.corroborate(ctx, provider, inv, mandate, refused, now, actor, r)
	default:
		r.Ambiguous++
		return s.leaveCharging(ctx, inv, refused.Code, refused.Message, refused)
	}
}

// holdOrWriteOff settles an invoice with no live mandate to charge. Only a mandate
// that has ended writes it off: a deal's waits for a card (§19.15), a paused mandate
// can resume, and a status pug has no word for proves nothing.
func (s *Service) holdOrWriteOff(
	ctx context.Context, inv dbread.ListDueBillingInvoicesRow, subs []Subscription, unnamed int, now time.Time,
	actor string, r *ChargeReport,
) error {
	var mandates, paused int
	for _, sub := range subs {
		if sub.OnDemand {
			mandates++
			if !ended(sub.Status) {
				paused++
			}
		}
	}
	switch {
	case unnamed > 0:
		r.Unreadable++
		err := fmt.Errorf("billing: org %s holds a mandate in a status pug has no word for", inv.OrgID)
		slog.ErrorContext(ctx, "an invoice waits on a mandate pug cannot read", slogx.Error(err),
			slog.String("org_id", inv.OrgID), slog.String("invoice_id", inv.ID))
		telemetry.RecordError(ctx, err)
		return nil
	case inv.PlanSlug == SlugCustom || mandates == 0:
		r.AwaitingCard++
		return nil
	case paused > 0:
		r.MandatePaused++
		return nil
	}
	return s.writeOff(ctx, inv, InvoiceStatus(inv.Status), "no live mandate to charge", now, actor, r)
}

// corroborate writes an invoice off on a not-chargeable refusal only once the mandate
// also reads back ended, and never a deal's. A second 404 proves nothing: a wrong
// environment answers both.
func (s *Service) corroborate(
	ctx context.Context, provider PaymentProvider, inv dbread.ListDueBillingInvoicesRow,
	mandate Subscription, refused *ChargeError, now time.Time, actor string, r *ChargeReport,
) error {
	event, err := provider.FetchSubscription(ctx, mandate.ProviderSubID)
	if err == nil && event.IsZero() {
		err = fmt.Errorf("billing: subscription %s reads back as nothing", mandate.ProviderSubID)
	}
	if err != nil {
		r.Unreadable++
		return s.leaveCharging(ctx, inv, refused.Code, refused.Message, err)
	}
	if status, ok := ParseSubStatus(string(event.Status)); !ok || !ended(status) || inv.PlanSlug == SlugCustom {
		r.Ambiguous++
		return s.leaveCharging(ctx, inv, refused.Code, refused.Message,
			fmt.Errorf("%w, but the mandate reads back %q", refused, event.Status))
	}
	ctx, cancel := recording(ctx)
	defer cancel()
	return s.writeOff(ctx, inv, InvoiceCharging, refused.Message, now, actor, r)
}

// recording detaches a write that records a provider's answer: money may have moved,
// so it must not die with the pass.
func recording(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
}

// ended is a mandate that can never be charged again.
func ended(status SubStatus) bool {
	switch status {
	case SubStatusCancelled, SubStatusExpired, SubStatusFailed:
		return true
	case SubStatusActive, SubStatusPastDue, SubStatusPaused:
		return false
	}
	return false
}

// leaveCharging records a charge that only a read can settle.
func (s *Service) leaveCharging(
	ctx context.Context, inv dbread.ListDueBillingInvoicesRow, code, message string, cause error,
) error {
	ctx, cancel := recording(ctx)
	defer cancel()
	slog.ErrorContext(ctx, "a charge is left unsettled", slogx.Error(cause),
		slog.String("org_id", inv.OrgID), slog.String("invoice_id", inv.ID))
	telemetry.RecordError(ctx, cause)
	n, err := s.write().RecordBillingInvoiceChargeError(ctx, dbwrite.RecordBillingInvoiceChargeErrorParams{
		ID: inv.ID, LastErrorCode: code, LastErrorMessage: message,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to record a charge error", slogx.Error(err),
			slog.String("org_id", inv.OrgID), slog.String("invoice_id", inv.ID))
		telemetry.RecordError(ctx, err)
		return err
	}
	if n == 0 {
		slog.WarnContext(ctx, "an invoice moved before its charge error was recorded",
			slog.String("org_id", inv.OrgID), slog.String("invoice_id", inv.ID))
	}
	return nil
}

func (s *Service) writeOff(
	ctx context.Context, inv dbread.ListDueBillingInvoicesRow, from InvoiceStatus, message string, now time.Time,
	actor string, r *ChargeReport,
) error {
	moved, err := s.moveInvoice(ctx, inv.OrgID, inv.ID, actor, codeMandateGone,
		func(w *dbwrite.Queries) (dbwrite.BillingInvoice, error) {
			return w.MarkBillingInvoiceUncollectible(ctx, dbwrite.MarkBillingInvoiceUncollectibleParams{
				FailedAt:         postgres.NewTimestamptz(now),
				FromStatus:       string(from),
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
	ctx context.Context, orgID, invoiceID, actor, detail string,
	update func(*dbwrite.Queries) (dbwrite.BillingInvoice, error),
) (bool, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	w := dbwrite.New(tx)
	cur, err := w.GetBillingInvoiceForUpdate(ctx, invoiceID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to lock an invoice", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("invoice_id", invoiceID))
		telemetry.RecordError(ctx, err)
		return false, err
	}
	row, err := update(w)
	if errors.Is(err, pgx.ErrNoRows) {
		slog.WarnContext(ctx, "an invoice was not in a state this write moves", slog.String("org_id", orgID),
			slog.String("invoice_id", invoiceID), slog.String("status", cur.Status), slog.String("detail", detail))
		return false, nil
	}
	if err != nil {
		slog.ErrorContext(ctx, "failed to move an invoice", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("invoice_id", invoiceID))
		telemetry.RecordError(ctx, err)
		return false, err
	}
	if err := appendInvoiceEvent(ctx, w, orgID, invoiceID, actor, InvoiceStatus(cur.Status), InvoiceStatus(row.Status), detail); err != nil {
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
	return desc + fmt.Sprintf(", and %s carried from %d earlier %s", usd(inv.CarriedCents), inv.CarriedPeriods, periods)
}
