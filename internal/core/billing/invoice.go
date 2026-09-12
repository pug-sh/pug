package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	coreusage "github.com/pug-sh/pug/internal/core/usage"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/rs/xid"
)

// InvoiceStatus is a recorded state machine: a charge is an event, not a
// comparison, so unlike the entitlement it is stored.
type InvoiceStatus string

const (
	InvoiceOpen          InvoiceStatus = "open"
	InvoiceCharging      InvoiceStatus = "charging"
	InvoiceCharged       InvoiceStatus = "charged"
	InvoicePaid          InvoiceStatus = "paid"
	InvoiceFailed        InvoiceStatus = "failed"
	InvoiceUncollectible InvoiceStatus = "uncollectible"
	InvoiceWaived        InvoiceStatus = "waived"
	InvoiceVoid          InvoiceStatus = "void"
	InvoiceRefunded      InvoiceStatus = "refunded"
)

// ParseInvoiceStatus refuses a status this build does not know, so a row written
// by a newer binary cannot silently take a branch meant for another state.
func ParseInvoiceStatus(s string) (InvoiceStatus, bool) {
	for _, st := range AllInvoiceStatuses() {
		if string(st) == s {
			return st, true
		}
	}
	return "", false
}

func AllInvoiceStatuses() []InvoiceStatus {
	return []InvoiceStatus{
		InvoiceOpen, InvoiceCharging, InvoiceCharged, InvoicePaid, InvoiceFailed,
		InvoiceUncollectible, InvoiceWaived, InvoiceVoid, InvoiceRefunded,
	}
}

var (
	ErrInvoiceNotFound = errors.New("billing: invoice not found")
	// ErrInvoiceTransition is a move the state machine does not allow from where
	// the invoice is.
	ErrInvoiceTransition = errors.New("billing: the invoice is not in a state that allows this")
)

const (
	// MinChargeCents is the floor under which an invoice is waived rather than
	// sent: 50 cents, well under the rate card's smallest non-zero invoice.
	MinChargeCents   = 50
	ActorInvoicePass = "invoice-pass"
	MaxInvoiceRows   = 120

	maxChargeAttempts   = 4
	settleChargingAfter = 5 * time.Minute
	pollChargedAfter    = time.Hour
	invoicePageSize     = 500
	// maxClosePeriods bounds how far back a stalled pass catches up.
	maxClosePeriods = 3
	// unbilledLookback bounds how long a closed period is re-reported.
	unbilledLookback = 35 * 24 * time.Hour
)

// retryBackoff is the provider's own recommendation: retries at +3d, +7d, +7d
// after the first failure.
var retryBackoff = []time.Duration{3 * 24 * time.Hour, 7 * 24 * time.Hour, 7 * 24 * time.Hour}

// hardDecline is a decline that retrying only damages authorization rates.
func hardDecline(code string) bool {
	switch code {
	case "STOLEN_CARD", "LOST_CARD", "DO_NOT_HONOR", "FRAUDULENT":
		return true
	}
	return false
}

type Invoice struct {
	ID            string
	OrgID         string
	Provider      string
	ProviderSubID string
	PlanSlug      string
	Pricing       Pricing

	PeriodStart time.Time
	PeriodEnd   time.Time
	BilledFrom  time.Time
	BilledTo    time.Time

	EventCount  int64
	Blocks      int64
	Lines       []Line
	AmountCents int64
	Currency    string

	Status        InvoiceStatus
	Attempts      int
	NextAttemptAt time.Time
	LastErrorCode string
	// LastErrorMessage is merchant-facing and never crosses the wire.
	LastErrorMessage string

	ProviderPaymentID  string
	ProviderInvoiceURL string
	UsageComputedAt    time.Time
	PaidAt             time.Time
	FailedAt           time.Time
	CreateTime         time.Time
	UpdateTime         time.Time
}

func invoiceFromRow(row dbread.BillingInvoice) (Invoice, error) {
	status, ok := ParseInvoiceStatus(row.Status)
	if !ok {
		return Invoice{}, fmt.Errorf("billing: invoice %s has status %q", row.ID, row.Status)
	}
	inv := Invoice{
		ID:                 row.ID,
		OrgID:              row.OrgID,
		Provider:           row.Provider.String,
		ProviderSubID:      row.ProviderSubID.String,
		PlanSlug:           row.PlanSlug,
		PeriodStart:        row.PeriodStart.Time,
		PeriodEnd:          row.PeriodEnd.Time,
		BilledFrom:         row.BilledFrom.Time,
		BilledTo:           row.BilledTo.Time,
		EventCount:         row.EventCount,
		Blocks:             row.Blocks,
		AmountCents:        row.AmountCents,
		Currency:           row.Currency,
		Status:             status,
		Attempts:           int(row.Attempts),
		NextAttemptAt:      row.NextAttemptAt.Time,
		LastErrorCode:      row.LastErrorCode,
		LastErrorMessage:   row.LastErrorMessage,
		ProviderPaymentID:  row.ProviderPaymentID.String,
		ProviderInvoiceURL: row.ProviderInvoiceUrl.String,
		UsageComputedAt:    row.UsageComputedAt.Time,
		PaidAt:             row.PaidAt.Time,
		FailedAt:           row.FailedAt.Time,
		CreateTime:         row.CreateTime.Time,
		UpdateTime:         row.UpdateTime.Time,
	}
	if err := json.Unmarshal(row.Lines, &inv.Lines); err != nil {
		return Invoice{}, fmt.Errorf("billing: invoice %s lines: %w", row.ID, err)
	}
	if err := json.Unmarshal(row.Pricing, &inv.Pricing); err != nil {
		return Invoice{}, fmt.Errorf("billing: invoice %s pricing: %w", row.ID, err)
	}
	return inv, nil
}

// InvoiceReport is what one invoicing pass did and found.
type InvoiceReport struct {
	Closed      int
	Waived      int
	Held        int
	Unpriceable int

	Charged     int
	Declined    int
	Ambiguous   int
	MandateGone int

	Settled  int
	Reopened int
	Pinned   int
	Unbilled int
	// Foreign is an invoice stamped with another provider: this pass cannot touch
	// it, and without a count it would be skipped every hour in silence.
	Foreign int
	// Unreadable is a provider call that failed: the only counter that can fail
	// the CronJob.
	Unreadable int
}

// InvoicePass closes due periods, charges, settles, pins and reports, in that
// order. Postgres failures return; provider failures are counted.
func (s *Service) InvoicePass(ctx context.Context, now time.Time) (InvoiceReport, error) {
	var r InvoiceReport
	if !s.cfg.Enabled || !s.payments.configured() {
		slog.InfoContext(ctx, "billing is off or has no provider; nothing to invoice")
		return r, nil
	}
	for _, step := range []func(context.Context, time.Time, *InvoiceReport) error{
		s.closePeriods, s.chargeDue, s.settle, s.pinNextCharges, s.reportUnbilled,
	} {
		if err := step(ctx, now, &r); err != nil {
			return r, err
		}
	}
	slog.InfoContext(ctx, "billing invoice pass finished",
		slog.Int("closed", r.Closed), slog.Int("waived", r.Waived), slog.Int("held", r.Held),
		slog.Int("unpriceable", r.Unpriceable), slog.Int("charged", r.Charged),
		slog.Int("declined", r.Declined), slog.Int("ambiguous", r.Ambiguous),
		slog.Int("mandate_gone", r.MandateGone), slog.Int("settled", r.Settled),
		slog.Int("reopened", r.Reopened), slog.Int("pinned", r.Pinned),
		slog.Int("unbilled", r.Unbilled), slog.Int("foreign", r.Foreign),
		slog.Int("unreadable", r.Unreadable))
	return r, nil
}

func (s *Service) closePeriods(ctx context.Context, now time.Time, r *InvoiceReport) error {
	orgs, err := s.read.ListBillingInvoiceOrgs(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list orgs to invoice", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}
	for _, org := range orgs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.closeOrg(ctx, org, now, r); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) closeOrg(ctx context.Context, org dbread.ListBillingInvoiceOrgsRow, now time.Time, r *InvoiceReport) error {
	rec, err := s.StoredRecord(ctx, org.ID)
	if err != nil {
		return err
	}
	subs, err := s.subscriptionsOf(ctx, org.ID)
	if err != nil {
		return err
	}
	anchor := coreusage.AnchorDay(org.CreateTime.Time, postgres.Int2ToInt(org.AnchorDay))
	grace := s.cfg.Grace()

	// A scheduled cancellation: invoice the period to date while the mandate is live.
	if live := liveOf(subs); live != nil && live.CancelAtPeriodEnd {
		start, end := coreusage.PeriodFor(now, anchor)
		if to := coreusage.FloorDayUTC(now.Add(-grace)); to.After(start) {
			if _, err := s.closeWindow(ctx, org.ID, org.CreateTime.Time, rec, live, start, end, to, now, r); err != nil {
				return err
			}
		}
	}

	start, _ := coreusage.PeriodFor(now, anchor)
	for range maxClosePeriods {
		end := start
		start, _ = coreusage.PeriodFor(end.Add(-time.Nanosecond), anchor)
		if !end.After(org.CreateTime.Time) {
			break
		}
		if now.Before(end.Add(grace)) {
			continue
		}
		sub := mandateOverlapping(subs, start, end)
		if _, err := s.closeWindow(ctx, org.ID, org.CreateTime.Time, rec, sub, start, end, end, end.Add(-time.Nanosecond), r); err != nil {
			return err
		}
	}
	return nil
}

// closeCurrentPeriod invoices the running period through billedTo: the early
// close of a cancellation.
func (s *Service) closeCurrentPeriod(ctx context.Context, orgID string, sub *Subscription, billedTo, now time.Time, r *InvoiceReport) (*Invoice, error) {
	row, err := dbread.New(s.pgW).GetOrgEntitlement(ctx, orgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrOrgNotFound
		}
		slog.ErrorContext(ctx, "failed to read the org for an early close", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return nil, err
	}
	rec := recordFromRow(row)
	start, end := coreusage.PeriodFor(now, coreusage.AnchorDay(row.OrgCreateTime.Time, rec.AnchorDay))
	// Nothing final to bill yet: a waived row here would block the real close.
	if !billedTo.After(start) {
		return nil, nil
	}
	return s.closeWindow(ctx, orgID, row.OrgCreateTime.Time, rec, sub, start, end, billedTo, now, r)
}

// closeWindow writes one invoice for [start, end), billing the days from the
// mandate's first through billedTo, trial days excluded, priced as of `at`.
func (s *Service) closeWindow(
	ctx context.Context, orgID string, orgCreate time.Time, rec Record, sub *Subscription,
	start, end, billedTo, at time.Time, r *InvoiceReport,
) (*Invoice, error) {
	exists, err := s.read.ExistsBillingInvoiceForPeriod(ctx, dbread.ExistsBillingInvoiceForPeriodParams{
		OrgID:       orgID,
		PeriodStart: postgres.NewTimestamptz(start),
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to check for an invoice", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return nil, err
	}
	if exists {
		return nil, nil
	}
	// A deal with no mandate is invoiced from the day it was recorded; a free org
	// gets no invoice at all.
	_, deal := rec.Terms()
	if sub == nil && (!deal || contractLapsed(rec, at) || !end.After(rec.CreateTime)) {
		return nil, nil
	}

	// Priced on the mandate as it stood: a cancelled row still pins its card.
	var pricingSub *Subscription
	if sub != nil {
		live := *sub
		live.Status = SubStatusActive
		pricingSub = &live
	}
	ent := Resolve(orgCreate, rec, pricingSub, at, true)
	pricing := ent.Pricing()
	if pricing.IsZero() {
		r.Unpriceable++
		err := fmt.Errorf("billing: no pricing for plan %q", ent.Slug)
		slog.ErrorContext(ctx, "period cannot be priced; holding the invoice", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("plan_slug", ent.Slug), slog.Time("period_start", start))
		telemetry.RecordError(ctx, err)
		return nil, nil
	}

	from := coreusage.FloorDayUTC(start)
	to := coreusage.FloorDayUTC(billedTo)
	if sub != nil {
		from = maxTime(from, coreusage.FloorDayUTC(sub.CreateTime))
		if ended := sub.endedAt(); !ended.IsZero() {
			to = minTime(to, coreusage.FloorDayUTC(ended))
		}
	} else {
		from = maxTime(from, coreusage.FloorDayUTC(rec.CreateTime))
	}
	from = maxTime(from, coreusage.CeilDayUTC(ent.TrialEndsAt))

	// A stalled meter delays an invoice, never mis-bills one. The stamp is the
	// org's last run, not this period's own row: the meter only keeps the current
	// period, so that row stops moving at the rollover and never clears the grace.
	latest, err := s.read.GetLatestUsageComputedAt(ctx, orgID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.ErrorContext(ctx, "failed to read the usage stamp", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return nil, err
	}
	if err != nil || latest.Time.Before(to.Add(s.cfg.Grace())) {
		r.Held++
		slog.WarnContext(ctx, "usage is not final yet; holding the invoice",
			slog.String("org_id", orgID), slog.Time("period_start", start), slog.Time("usage_computed_at", latest.Time))
		return nil, nil
	}

	var events int64
	if from.Before(to) {
		events, err = s.read.SumUsageDaily(ctx, dbread.SumUsageDailyParams{
			OrgID: orgID, FromDay: postgres.NewDate(from), ToDay: postgres.NewDate(to),
		})
		if err != nil {
			slog.ErrorContext(ctx, "failed to sum usage for an invoice", slogx.Error(err), slog.String("org_id", orgID))
			telemetry.RecordError(ctx, err)
			return nil, err
		}
	} else {
		to = from
	}
	var quote Quote
	if from.Before(to) {
		quote = pricing.Quote(events)
	}
	status := InvoiceOpen
	if quote.TotalCents < MinChargeCents {
		status = InvoiceWaived
	}
	lines, err := json.Marshal(nonNil(quote.Lines))
	if err != nil {
		return nil, err
	}
	pricingJSON, err := json.Marshal(pricing)
	if err != nil {
		return nil, err
	}
	params := dbwrite.InsertBillingInvoiceParams{
		AmountCents:     quote.TotalCents,
		BilledFrom:      postgres.NewDate(from),
		BilledTo:        postgres.NewDate(to),
		Blocks:          quote.Blocks,
		Currency:        ent.Currency,
		EventCount:      events,
		ID:              xid.New().String(),
		Lines:           lines,
		OrgID:           orgID,
		PeriodEnd:       postgres.NewTimestamptz(end),
		PeriodStart:     postgres.NewTimestamptz(start),
		PlanSlug:        ent.Slug,
		Pricing:         pricingJSON,
		Status:          string(status),
		UsageComputedAt: latest,
	}
	if status == InvoiceOpen {
		params.NextAttemptAt = postgres.NewTimestamptz(at)
	}
	if sub != nil {
		params.Provider = postgres.NewOptionalText(sub.Provider)
		params.ProviderSubID = postgres.NewOptionalText(sub.ProviderSubID)
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	w := dbwrite.New(tx)
	row, err := w.InsertBillingInvoice(ctx, params)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		slog.ErrorContext(ctx, "failed to insert an invoice", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return nil, err
	}
	if err := w.InsertBillingInvoiceEvent(ctx, dbwrite.InsertBillingInvoiceEventParams{
		Actor: ActorInvoicePass, ID: xid.New().String(), InvoiceID: row.ID, ToStatus: string(status),
	}); err != nil {
		slog.ErrorContext(ctx, "failed to record an invoice event", slogx.Error(err), slog.String("invoice_id", row.ID))
		telemetry.RecordError(ctx, err)
		return nil, err
	}
	if err := s.commit(ctx, tx, orgID); err != nil {
		return nil, err
	}
	inv, err := invoiceFromRow(dbread.BillingInvoice(row))
	if err != nil {
		return nil, err
	}
	if status == InvoiceWaived {
		r.Waived++
	} else {
		r.Closed++
	}
	return &inv, nil
}

func (s *Service) chargeDue(ctx context.Context, now time.Time, r *InvoiceReport) error {
	rows, err := dbread.New(s.pgW).ListDueBillingInvoices(ctx, dbread.ListDueBillingInvoicesParams{
		Now: postgres.NewTimestamptz(now), RowLimit: invoicePageSize,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to list due invoices", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		inv, err := invoiceFromRow(row)
		if err != nil {
			slog.ErrorContext(ctx, "invoice row does not decode", slogx.Error(err), slog.String("invoice_id", row.ID))
			telemetry.RecordError(ctx, err)
			r.Unreadable++
			continue
		}
		if err := s.chargeOne(ctx, s.payments.Provider, inv, now, r); err != nil {
			return err
		}
	}
	return nil
}

// chargeOne runs one charge: the row goes to charging and COMMITS before the
// provider is called, so a process dying in between leaves a row that says so.
func (s *Service) chargeOne(ctx context.Context, provider PaymentProvider, inv Invoice, now time.Time, r *InvoiceReport) error {
	sub, err := s.liveSubscription(ctx, inv.OrgID)
	if err != nil {
		return err
	}
	if sub == nil || !sub.OnDemand {
		if _, err := s.uncollectible(ctx, inv, "mandate_gone", "no live payment method", "", false, now, ActorInvoicePass); err != nil {
			return err
		}
		r.MandateGone++
		return nil
	}
	if sub.Provider != provider.Name() {
		r.Foreign++
		return nil
	}
	inv, err = s.transition(ctx, inv.ID, ActorInvoicePass, "charging", func(w *dbwrite.Queries) (dbwrite.BillingInvoice, error) {
		return w.MarkBillingInvoiceCharging(ctx, dbwrite.MarkBillingInvoiceChargingParams{
			ID:            inv.ID,
			Provider:      postgres.NewOptionalText(sub.Provider),
			ProviderSubID: postgres.NewOptionalText(sub.ProviderSubID),
		})
	})
	if err != nil {
		if errors.Is(err, ErrInvoiceTransition) {
			return nil
		}
		return err
	}

	paymentID, err := provider.Charge(ctx, ChargeInput{
		ProviderSubID: sub.ProviderSubID,
		AmountCents:   inv.AmountCents,
		Currency:      inv.Currency,
		Description:   chargeDescription(inv),
		InvoiceID:     inv.ID,
		OrgID:         inv.OrgID,
		PeriodStart:   inv.PeriodStart,
	})
	var decline *DeclineError
	switch {
	case err == nil:
		if _, err := s.transition(ctx, inv.ID, ActorInvoicePass, "payment "+paymentID, func(w *dbwrite.Queries) (dbwrite.BillingInvoice, error) {
			return w.MarkBillingInvoiceCharged(ctx, dbwrite.MarkBillingInvoiceChargedParams{
				ID: inv.ID, ProviderPaymentID: postgres.NewOptionalText(paymentID),
			})
		}); err != nil {
			return err
		}
		r.Charged++
	case errors.As(err, &decline):
		if err := s.recordDecline(ctx, inv, decline.Code, decline.Message, "", true, now, ActorInvoicePass); err != nil {
			return err
		}
		r.Declined++
	case errors.Is(err, ErrMandateNotChargeable):
		if _, err := s.uncollectible(ctx, inv, "mandate_gone", err.Error(), "", true, now, ActorInvoicePass); err != nil {
			return err
		}
		r.MandateGone++
	default:
		// Left in charging: settled by reading, never by charging again on a hunch.
		slog.WarnContext(ctx, "charge outcome is unknown; will settle by reading", slogx.Error(err),
			slog.String("invoice_id", inv.ID), slog.String("org_id", inv.OrgID))
		r.Ambiguous++
	}
	return nil
}

func chargeDescription(inv Invoice) string {
	return fmt.Sprintf("Pug: %s events, %s to %s", formatEvents(inv.EventCount),
		inv.BilledFrom.Format("2 Jan"), inv.BilledTo.AddDate(0, 0, -1).Format("2 Jan 2006"))
}

func formatEvents(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.0fk", float64(n)/1_000)
	}
	return strconv.FormatInt(n, 10)
}

// recordDecline applies the dunning policy: hard declines and the fourth failure
// are uncollectible, the rest retry on the backoff schedule.
func (s *Service) recordDecline(ctx context.Context, inv Invoice, code, message, paymentID string, countAttempt bool, now time.Time, actor string) error {
	attempts := inv.Attempts
	if countAttempt {
		attempts++
	}
	if hardDecline(code) || attempts >= maxChargeAttempts {
		_, err := s.uncollectible(ctx, inv, code, message, paymentID, countAttempt, now, actor)
		return err
	}
	next := now.Add(retryBackoff[min(max(attempts-1, 0), len(retryBackoff)-1)])
	_, err := s.transition(ctx, inv.ID, actor, "declined: "+code, func(w *dbwrite.Queries) (dbwrite.BillingInvoice, error) {
		return w.MarkBillingInvoiceFailed(ctx, dbwrite.MarkBillingInvoiceFailedParams{
			ID:                inv.ID,
			FailedAt:          postgres.NewTimestamptz(now),
			NextAttemptAt:     postgres.NewTimestamptz(next),
			LastErrorCode:     code,
			LastErrorMessage:  message,
			CountAttempt:      boolInt(countAttempt),
			ProviderPaymentID: paymentID,
		})
	})
	if errors.Is(err, ErrInvoiceTransition) {
		return nil
	}
	return err
}

func (s *Service) uncollectible(ctx context.Context, inv Invoice, code, message, paymentID string, countAttempt bool, now time.Time, actor string) (Invoice, error) {
	out, err := s.transition(ctx, inv.ID, actor, "uncollectible: "+code, func(w *dbwrite.Queries) (dbwrite.BillingInvoice, error) {
		return w.MarkBillingInvoiceUncollectible(ctx, dbwrite.MarkBillingInvoiceUncollectibleParams{
			ID:                inv.ID,
			FailedAt:          postgres.NewTimestamptz(now),
			LastErrorCode:     code,
			LastErrorMessage:  message,
			CountAttempt:      boolInt(countAttempt),
			ProviderPaymentID: paymentID,
		})
	})
	if errors.Is(err, ErrInvoiceTransition) {
		return inv, nil
	}
	return out, err
}

// settle resolves what the charge response could not: charging rows by listing
// the mandate's payments, charged rows by polling the payment.
func (s *Service) settle(ctx context.Context, now time.Time, r *InvoiceReport) error {
	provider := s.payments.Provider
	charging, err := s.invoicesByStatusBefore(ctx, InvoiceCharging, now.Add(-settleChargingAfter))
	if err != nil {
		return err
	}
	for _, inv := range charging {
		if err := ctx.Err(); err != nil {
			return err
		}
		if inv.Provider != provider.Name() {
			r.Foreign++
			continue
		}
		payments, err := provider.ListPayments(ctx, inv.ProviderSubID, inv.CreateTime.Add(-time.Hour))
		if err != nil {
			r.Unreadable++
			slog.ErrorContext(ctx, "failed to list payments to settle a charge", slogx.Error(err),
				slog.String("invoice_id", inv.ID))
			telemetry.RecordError(ctx, err)
			continue
		}
		found := pickPayment(payments, inv.ID)
		if found == nil {
			// The charge never arrived, so retrying at once is right -- but the reopen
			// counts an attempt, and at the cap it stops rather than cycling forever.
			if inv.Attempts+1 >= maxChargeAttempts {
				if _, err := s.uncollectible(ctx, inv, "unsettled", "no payment found for the charge", "", true, now, ActorInvoicePass); err != nil {
					return err
				}
				r.Declined++
				continue
			}
			_, err := s.transition(ctx, inv.ID, ActorInvoicePass, "no payment found; reopened", func(w *dbwrite.Queries) (dbwrite.BillingInvoice, error) {
				return w.MarkBillingInvoiceOpen(ctx, dbwrite.MarkBillingInvoiceOpenParams{
					ID: inv.ID, NextAttemptAt: postgres.NewTimestamptz(now),
				})
			})
			if err != nil && !errors.Is(err, ErrInvoiceTransition) {
				return err
			}
			r.Reopened++
			continue
		}
		rec := *found
		if rec.Status == PaymentFailed && rec.ErrorCode == "" {
			// The list carries no reason, and dunning branches on it: a hard decline
			// must not be retried three more times because the code arrived empty.
			if full, err := provider.FetchPayment(ctx, rec.PaymentID); err == nil {
				rec = full
			} else {
				slog.WarnContext(ctx, "failed to read a declined payment's reason", slogx.Error(err),
					slog.String("invoice_id", inv.ID), slog.String("payment_id", rec.PaymentID))
			}
		}
		if err := s.applyPaymentOutcome(ctx, inv, rec, now, ActorInvoicePass); err != nil {
			return err
		}
		r.Settled++
	}

	charged, err := s.invoicesByStatusBefore(ctx, InvoiceCharged, now.Add(-pollChargedAfter))
	if err != nil {
		return err
	}
	for _, inv := range charged {
		if err := ctx.Err(); err != nil {
			return err
		}
		if inv.Provider != provider.Name() {
			r.Foreign++
			continue
		}
		rec, err := provider.FetchPayment(ctx, inv.ProviderPaymentID)
		if errors.Is(err, ErrPaymentNotFound) {
			if err := s.recordDecline(ctx, inv, "payment_gone", "the provider does not know this payment", "", true, now, ActorInvoicePass); err != nil {
				return err
			}
			r.Reopened++
			continue
		}
		if err != nil {
			r.Unreadable++
			slog.ErrorContext(ctx, "failed to poll a payment", slogx.Error(err), slog.String("invoice_id", inv.ID))
			telemetry.RecordError(ctx, err)
			continue
		}
		if rec.Status == PaymentPending {
			r.Held++
			slog.WarnContext(ctx, "a charged payment is still pending", slog.String("invoice_id", inv.ID),
				slog.String("payment_id", inv.ProviderPaymentID))
			continue
		}
		if err := s.applyPaymentOutcome(ctx, inv, rec, now, ActorInvoicePass); err != nil {
			return err
		}
		r.Settled++
	}
	return nil
}

// pickPayment is the payment an invoice settles on: a succeeded one beats a
// failed one, and the newest beats an earlier attempt. ListPayments promises no
// order, so taking the first match can settle on the attempt before the retry.
func pickPayment(payments []PaymentRecord, invoiceID string) *PaymentRecord {
	var best *PaymentRecord
	for i := range payments {
		p := &payments[i]
		if p.InvoiceID != invoiceID {
			continue
		}
		switch {
		case best == nil,
			best.Status != PaymentSucceeded && p.Status == PaymentSucceeded,
			(best.Status == PaymentSucceeded) == (p.Status == PaymentSucceeded) && p.CreatedAt.After(best.CreatedAt):
			best = p
		}
	}
	return best
}

// applyPaymentOutcome moves an invoice on what the provider says about its
// payment; shared by the webhook and the settle step.
func (s *Service) applyPaymentOutcome(ctx context.Context, inv Invoice, rec PaymentRecord, now time.Time, actor string) error {
	switch rec.Status {
	case PaymentSucceeded:
		// The money moved, so the invoice is paid either way -- but pug billed an
		// amount and nothing else checks that it is the one that was taken.
		if rec.AmountCents != 0 && (rec.AmountCents != inv.AmountCents ||
			(rec.Currency != "" && !strings.EqualFold(rec.Currency, inv.Currency))) {
			err := fmt.Errorf("billing: invoice %s billed %d %s but %s took %d %s",
				inv.ID, inv.AmountCents, inv.Currency, rec.PaymentID, rec.AmountCents, rec.Currency)
			slog.ErrorContext(ctx, "a payment does not match the invoice it settles", slogx.Error(err),
				slog.String("invoice_id", inv.ID), slog.String("payment_id", rec.PaymentID))
			telemetry.RecordError(ctx, err)
		}
		_, err := s.transition(ctx, inv.ID, actor, "paid", func(w *dbwrite.Queries) (dbwrite.BillingInvoice, error) {
			return w.MarkBillingInvoicePaid(ctx, dbwrite.MarkBillingInvoicePaidParams{
				ID:                 inv.ID,
				PaidAt:             postgres.NewTimestamptz(now),
				ProviderPaymentID:  postgres.NewOptionalText(rec.PaymentID),
				ProviderInvoiceUrl: rec.InvoiceURL,
			})
		})
		if errors.Is(err, ErrInvoiceTransition) {
			return nil
		}
		return err
	case PaymentFailed:
		return s.recordDecline(ctx, inv, rec.ErrorCode, rec.ErrorMessage, rec.PaymentID, inv.Status == InvoiceCharging, now, actor)
	case PaymentPending:
	}
	if inv.Status != InvoiceCharging {
		return nil
	}
	_, err := s.transition(ctx, inv.ID, actor, "payment "+rec.PaymentID, func(w *dbwrite.Queries) (dbwrite.BillingInvoice, error) {
		return w.MarkBillingInvoiceCharged(ctx, dbwrite.MarkBillingInvoiceChargedParams{
			ID: inv.ID, ProviderPaymentID: postgres.NewOptionalText(rec.PaymentID),
		})
	})
	if errors.Is(err, ErrInvoiceTransition) {
		return nil
	}
	return err
}

// pinNextCharges moves each mandate's next_billing_date to just after pug's next
// charge, so a portal cancellation lands after the final invoice. A scheduled
// cancellation is left where it is, or it would never arrive.
func (s *Service) pinNextCharges(ctx context.Context, now time.Time, r *InvoiceReport) error {
	provider := s.payments.Provider
	for offset := int32(0); ; offset += invoicePageSize {
		rows, err := dbread.New(s.pgW).ListLiveBillingSubscriptionsByProvider(ctx,
			dbread.ListLiveBillingSubscriptionsByProviderParams{
				Provider: provider.Name(), RowLimit: invoicePageSize, RowOffset: offset,
			})
		if err != nil {
			slog.ErrorContext(ctx, "failed to list live mandates", slogx.Error(err))
			telemetry.RecordError(ctx, err)
			return err
		}
		for _, row := range rows {
			if err := ctx.Err(); err != nil {
				return err
			}
			sub, ok := subscriptionFromRow(row)
			if !ok || !sub.OnDemand || sub.CancelAtPeriodEnd {
				continue
			}
			org, err := s.read.GetOrgEntitlement(ctx, row.OrgID)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					continue
				}
				slog.ErrorContext(ctx, "failed to read the org for a pin", slogx.Error(err), slog.String("org_id", row.OrgID))
				telemetry.RecordError(ctx, err)
				return err
			}
			_, end := coreusage.PeriodFor(now, coreusage.AnchorDay(org.OrgCreateTime.Time, postgres.Int2ToInt(org.AnchorDay)))
			want := end.Add(s.cfg.Grace() + 24*time.Hour)
			if d := sub.CurrentPeriodEnd.Sub(want); d > -time.Hour && d < time.Hour {
				continue
			}
			if err := provider.SetNextBillingDate(ctx, sub.ProviderSubID, want); err != nil {
				r.Unreadable++
				slog.ErrorContext(ctx, "failed to pin a mandate's next billing date", slogx.Error(err),
					slog.String("org_id", row.OrgID), slog.String("provider_sub_id", sub.ProviderSubID))
				telemetry.RecordError(ctx, err)
				continue
			}
			event, err := provider.FetchSubscription(ctx, sub.ProviderSubID)
			if err != nil {
				r.Unreadable++
				slog.ErrorContext(ctx, "failed to re-read a pinned mandate", slogx.Error(err),
					slog.String("org_id", row.OrgID), slog.String("provider_sub_id", sub.ProviderSubID))
				telemetry.RecordError(ctx, err)
				continue
			}
			if !event.IsZero() {
				if _, err := s.applySubscription(ctx, provider, row.OrgID, event, now); err != nil {
					r.Unreadable++
					continue
				}
			}
			r.Pinned++
		}
		if len(rows) < invoicePageSize {
			return nil
		}
	}
}

// reportUnbilled is what the free tier costs: closed periods over the current
// allowance for orgs with no mandate.
func (s *Service) reportUnbilled(ctx context.Context, now time.Time, r *InvoiceReport) error {
	card := CurrentCard()
	rows, err := s.read.ListUnbilledUsagePeriods(ctx, dbread.ListUnbilledUsagePeriodsParams{
		MinEvents:    card.FreeBlocks * card.BlockEvents,
		ClosedBefore: postgres.NewTimestamptz(now.Add(-s.cfg.Grace())),
		Since:        postgres.NewTimestamptz(now.Add(-unbilledLookback)),
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to list unbilled usage", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}
	for _, row := range rows {
		r.Unbilled++
		slog.WarnContext(ctx, "usage over the free allowance with no mandate to bill",
			slog.String("org_id", row.OrgID), slog.Time("period_start", row.PeriodStart.Time),
			slog.Int64("event_count", row.EventCount))
	}
	return nil
}

// reopenDunning puts every failed and uncollectible invoice back in line: a new
// card arrived.
func (s *Service) reopenDunning(ctx context.Context, orgID, actor string, now time.Time) (int, error) {
	rows, err := s.write().ListDunningBillingInvoicesByOrg(ctx, orgID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list dunning invoices", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return 0, err
	}
	n := 0
	for _, row := range rows {
		_, err := s.transition(ctx, row.ID, actor, "payment method updated", func(w *dbwrite.Queries) (dbwrite.BillingInvoice, error) {
			return w.ReopenBillingInvoice(ctx, dbwrite.ReopenBillingInvoiceParams{
				ID: row.ID, NextAttemptAt: postgres.NewTimestamptz(now),
			})
		})
		if err != nil && !errors.Is(err, ErrInvoiceTransition) {
			return n, err
		}
		n++
	}
	return n, nil
}

// transition applies one state change and its event row in one transaction.
func (s *Service) transition(ctx context.Context, id, actor, detail string, apply func(*dbwrite.Queries) (dbwrite.BillingInvoice, error)) (Invoice, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return Invoice{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	w := dbwrite.New(tx)
	prev, err := w.GetBillingInvoiceForUpdate(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Invoice{}, ErrInvoiceNotFound
		}
		slog.ErrorContext(ctx, "failed to lock an invoice", slogx.Error(err), slog.String("invoice_id", id))
		telemetry.RecordError(ctx, err)
		return Invoice{}, err
	}
	row, err := apply(w)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			slog.WarnContext(ctx, "invoice transition refused", slog.String("invoice_id", id),
				slog.String("status", prev.Status), slog.String("detail", detail))
			return Invoice{}, fmt.Errorf("%w: %s is %s", ErrInvoiceTransition, id, prev.Status)
		}
		slog.ErrorContext(ctx, "failed to update an invoice", slogx.Error(err), slog.String("invoice_id", id))
		telemetry.RecordError(ctx, err)
		return Invoice{}, err
	}
	if err := w.InsertBillingInvoiceEvent(ctx, dbwrite.InsertBillingInvoiceEventParams{
		Actor: actor, Detail: detail, FromStatus: prev.Status, ID: xid.New().String(),
		InvoiceID: id, ToStatus: row.Status,
	}); err != nil {
		slog.ErrorContext(ctx, "failed to record an invoice event", slogx.Error(err), slog.String("invoice_id", id))
		telemetry.RecordError(ctx, err)
		return Invoice{}, err
	}
	if err := s.commit(ctx, tx, row.OrgID); err != nil {
		return Invoice{}, err
	}
	return invoiceFromRow(dbread.BillingInvoice(row))
}

func (s *Service) GetInvoice(ctx context.Context, id string) (Invoice, error) {
	row, err := dbread.New(s.pgW).GetBillingInvoice(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Invoice{}, ErrInvoiceNotFound
		}
		slog.ErrorContext(ctx, "failed to read an invoice", slogx.Error(err), slog.String("invoice_id", id))
		telemetry.RecordError(ctx, err)
		return Invoice{}, err
	}
	return invoiceFromRow(row)
}

func (s *Service) invoiceByPayment(ctx context.Context, provider, paymentID string) (Invoice, error) {
	row, err := dbread.New(s.pgW).GetBillingInvoiceByPayment(ctx, dbread.GetBillingInvoiceByPaymentParams{
		Provider: postgres.NewOptionalText(provider), ProviderPaymentID: postgres.NewOptionalText(paymentID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Invoice{}, ErrInvoiceNotFound
		}
		slog.ErrorContext(ctx, "failed to read an invoice by payment", slogx.Error(err), slog.String("payment_id", paymentID))
		telemetry.RecordError(ctx, err)
		return Invoice{}, err
	}
	return invoiceFromRow(row)
}

// ListInvoices is the org's ledger, newest first.
func (s *Service) ListInvoices(ctx context.Context, orgID string) ([]Invoice, error) {
	rows, err := dbread.New(s.pgW).ListBillingInvoicesByOrg(ctx, dbread.ListBillingInvoicesByOrgParams{
		OrgID: orgID, RowLimit: MaxInvoiceRows,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to list invoices", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return nil, err
	}
	out := make([]Invoice, 0, len(rows))
	for _, row := range rows {
		inv, err := invoiceFromRow(row)
		if err != nil {
			slog.ErrorContext(ctx, "invoice row does not decode", slogx.Error(err), slog.String("invoice_id", row.ID))
			telemetry.RecordError(ctx, err)
			return nil, err
		}
		out = append(out, inv)
	}
	return out, nil
}

func (s *Service) invoicesByStatusBefore(ctx context.Context, status InvoiceStatus, before time.Time) ([]Invoice, error) {
	rows, err := dbread.New(s.pgW).ListBillingInvoicesByStatusBefore(ctx, dbread.ListBillingInvoicesByStatusBeforeParams{
		Status: string(status), Before: postgres.NewTimestamptz(before), RowLimit: invoicePageSize,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to list invoices by status", slogx.Error(err), slog.String("status", string(status)))
		telemetry.RecordError(ctx, err)
		return nil, err
	}
	out := make([]Invoice, 0, len(rows))
	for _, row := range rows {
		inv, err := invoiceFromRow(row)
		if err != nil {
			slog.ErrorContext(ctx, "invoice row does not decode", slogx.Error(err), slog.String("invoice_id", row.ID))
			telemetry.RecordError(ctx, err)
			return nil, err
		}
		out = append(out, inv)
	}
	return out, nil
}

// VoidInvoice stops a charge before it happens, or records that a paid one was
// refunded by hand. It never calls the provider.
func (s *Service) VoidInvoice(ctx context.Context, id, actor, note string) (Invoice, error) {
	if actor == "" {
		return Invoice{}, ErrActorRequired
	}
	return s.transition(ctx, id, actor, note, func(w *dbwrite.Queries) (dbwrite.BillingInvoice, error) {
		return w.VoidBillingInvoice(ctx, id)
	})
}

// RetryInvoice is for the customer who fixed their card by phone.
func (s *Service) RetryInvoice(ctx context.Context, id, actor string, now time.Time) (Invoice, error) {
	if actor == "" {
		return Invoice{}, ErrActorRequired
	}
	return s.transition(ctx, id, actor, "retry", func(w *dbwrite.Queries) (dbwrite.BillingInvoice, error) {
		return w.ReopenBillingInvoice(ctx, dbwrite.ReopenBillingInvoiceParams{
			ID: id, NextAttemptAt: postgres.NewTimestamptz(now),
		})
	})
}

// Upcoming is the running period priced so far. Counted carries the meter's
// three states through; an unknown count is never rendered as $0.
type Upcoming struct {
	PeriodStart     time.Time
	PeriodEnd       time.Time
	NextChargeAt    time.Time
	UsageComputedAt time.Time
	Counted         bool
	EventCount      int64
	Quote           Quote
	Currency        string
	// Priced is false when the org's slug cannot be priced.
	Priced bool
}

func (s *Service) UpcomingInvoice(ctx context.Context, orgID string, now time.Time) (Upcoming, error) {
	ent, err := s.GetEntitlement(ctx, orgID, now)
	if err != nil {
		return Upcoming{}, err
	}
	usage, err := s.usage.GetPeriodUsage(ctx, orgID, ent.PeriodStart)
	if err != nil {
		return Upcoming{}, err
	}
	up := Upcoming{
		PeriodStart:     ent.PeriodStart,
		PeriodEnd:       ent.PeriodEnd,
		NextChargeAt:    ent.NextChargeAt,
		UsageComputedAt: usage.UsageComputedAt,
		Counted:         usage.Counted,
		Currency:        ent.Currency,
	}
	pricing := ent.Pricing()
	if !usage.Counted || pricing.IsZero() {
		return up, nil
	}
	up.EventCount = usage.EventCount
	up.Quote = pricing.Quote(usage.EventCount)
	up.Priced = true
	return up, nil
}

func (s *Service) subscriptionsOf(ctx context.Context, orgID string) ([]Subscription, error) {
	rows, err := dbread.New(s.pgW).ListBillingSubscriptionsByOrg(ctx, orgID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list the org's subscriptions", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return nil, err
	}
	out := make([]Subscription, 0, len(rows))
	for _, row := range rows {
		if sub, ok := subscriptionFromRow(row); ok {
			out = append(out, sub)
		}
	}
	return out, nil
}

func liveOf(subs []Subscription) *Subscription {
	for i := range subs {
		if subs[i].Status.Live() {
			return &subs[i]
		}
	}
	return nil
}

// mandateOverlapping is the mandate live at any point in [start, end): the one
// still live, or the newest that ended after the period began.
func mandateOverlapping(subs []Subscription, start, end time.Time) *Subscription {
	var best *Subscription
	for i := range subs {
		sub := &subs[i]
		if !sub.OnDemand || !sub.CreateTime.Before(end) {
			continue
		}
		if ended := sub.endedAt(); !ended.IsZero() && !ended.After(start) {
			continue
		}
		if best == nil || sub.CreateTime.After(best.CreateTime) {
			best = sub
		}
	}
	return best
}

// endedAt is when a mandate stopped: the provider's date, else when pug saw it
// leave the live set. Zero while live.
func (s Subscription) endedAt() time.Time {
	if s.Status.Live() {
		return time.Time{}
	}
	if !s.EndedAt.IsZero() {
		return s.EndedAt
	}
	return s.UpdateTime
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func boolInt(b bool) int32 {
	if b {
		return 1
	}
	return 0
}

func nonNil(lines []Line) []Line {
	if lines == nil {
		return []Line{}
	}
	return lines
}
