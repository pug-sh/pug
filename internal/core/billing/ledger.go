package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	coreusage "github.com/pug-sh/pug/internal/core/usage"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/slogx"
)

var (
	ErrBillingDisabled = errors.New("billing: billing is not enabled")
	// ErrInvoiceUndecodable is a stored invoice this build cannot read. It fails the whole
	// read rather than returning a history one row short.
	ErrInvoiceUndecodable = errors.New("billing: a stored invoice cannot be decoded")
	// ErrPlanUnpriceable is an org whose plan slug no card answers to. Only an operator
	// can clear it, so it is not an internal error.
	ErrPlanUnpriceable = errors.New("billing: this org's plan cannot be priced")
)

// UpcomingInvoice is the current period priced so far, and the balance earlier closes
// deferred. Quote is set only once the meter has counted the period, and counts the
// days the close will bill, which Usage does not.
type UpcomingInvoice struct {
	Usage        coreusage.PeriodUsage
	Quote        Quote
	CarriedCents int64
	Currency     string
}

// GetUpcomingInvoice prices the current period on the card or deal the org resolves to,
// over the days its invoice will bill, through the function that invoice is priced with.
func (s *Service) GetUpcomingInvoice(ctx context.Context, orgID string, now time.Time) (UpcomingInvoice, error) {
	if !s.billingEnabled {
		return UpcomingInvoice{}, ErrBillingDisabled
	}
	ent, row, err := s.resolve(ctx, orgID, now)
	if err != nil {
		return UpcomingInvoice{}, err
	}
	usage, err := s.usage.GetPeriodUsage(ctx, orgID, ent.PeriodStart)
	if err != nil {
		return UpcomingInvoice{}, err
	}
	carried, err := dbread.New(s.pgW).SumUncoveredDeferredBillingInvoices(ctx, orgID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to read the deferred balance", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return UpcomingInvoice{}, err
	}
	out := UpcomingInvoice{Usage: usage, CarriedCents: carried, Currency: ent.Currency}
	if !usage.Counted {
		return out, nil
	}
	events, err := s.billableEvents(ctx, orgID, row, ent)
	if err != nil {
		return UpcomingInvoice{}, err
	}
	quote, ok := ent.quote(events)
	if !ok {
		// Warn, not error: resolve already warned on the same load, and only an
		// operator can clear it.
		slog.WarnContext(ctx, "the current period cannot be priced",
			slog.String("org_id", orgID), slog.String("plan_slug", ent.Slug))
		return UpcomingInvoice{}, ErrPlanUnpriceable
	}
	out.Quote = quote
	return out, nil
}

// billableEvents is the period's events over the days its close will bill. Trial days,
// and days before the payment method, are counted by the meter and never charged.
func (s *Service) billableEvents(
	ctx context.Context, orgID string, row dbread.GetOrgEntitlementRow, ent Entitlement,
) (int64, error) {
	read := dbread.New(s.pgW)
	billed, err := read.GetBillingInvoiceBilledTo(ctx, orgID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to read where billing has reached", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return 0, err
	}
	subs, _, err := s.subscriptionsOf(ctx, orgID)
	if err != nil {
		return 0, err
	}
	segs := billableSegments(row.OrgCreateTime.Time, recordFromRow(row), subs,
		period{ent.PeriodStart, ent.PeriodEnd}, billed.Time)
	var events int64
	for _, seg := range segs {
		for _, d := range seg.days {
			n, err := read.SumUsageDaily(ctx, dbread.SumUsageDailyParams{
				OrgID: orgID, FromDay: postgres.NewDate(d.from), ToDay: postgres.NewDate(d.to),
			})
			if err != nil {
				slog.ErrorContext(ctx, "failed to sum the period's billable usage", slogx.Error(err),
					slog.String("org_id", orgID))
				telemetry.RecordError(ctx, err)
				return 0, err
			}
			events += n
		}
	}
	return events, nil
}

// Invoice is one close as the ledger holds it.
type Invoice struct {
	ID         string
	Status     InvoiceStatus
	Currency   string
	EventCount int64
	Lines      []Line

	UsageCents   int64
	CarriedCents int64
	AmountCents  int64
	TaxCents     *int64

	PeriodStart   time.Time
	PeriodEnd     time.Time
	BilledFrom    time.Time
	BilledTo      time.Time
	CreateTime    time.Time
	PaidAt        time.Time
	NextAttemptAt time.Time

	CoveredBy          string
	ProviderInvoiceURL string
}

// ListInvoices is the org's ledger, newest first.
func (s *Service) ListInvoices(ctx context.Context, orgID string) ([]Invoice, error) {
	if !s.billingEnabled {
		return nil, ErrBillingDisabled
	}
	rows, err := dbread.New(s.pgW).ListBillingInvoicesByOrg(ctx, orgID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list the org's invoices", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return nil, err
	}
	out := make([]Invoice, 0, len(rows))
	for _, row := range rows {
		inv, err := invoiceFromRow(row)
		if err != nil {
			slog.ErrorContext(ctx, "a stored invoice cannot be decoded", slogx.Error(err),
				slog.String("org_id", orgID), slog.String("invoice_id", row.ID))
			telemetry.RecordError(ctx, err)
			return nil, err
		}
		out = append(out, inv)
	}
	return out, nil
}

func invoiceFromRow(row dbread.ListBillingInvoicesByOrgRow) (Invoice, error) {
	status, ok := ParseInvoiceStatus(row.Status)
	if !ok {
		return Invoice{}, fmt.Errorf("%w: status %q", ErrInvoiceUndecodable, row.Status)
	}
	var lines []Line
	if err := json.Unmarshal(row.Lines, &lines); err != nil {
		return Invoice{}, fmt.Errorf("%w: lines: %w", ErrInvoiceUndecodable, err)
	}
	inv := Invoice{
		AmountCents:        row.AmountCents,
		BilledFrom:         row.BilledFrom.Time,
		BilledTo:           row.BilledTo.Time,
		CarriedCents:       row.CarriedCents,
		CoveredBy:          row.CoveredBy.String,
		CreateTime:         row.CreateTime.Time,
		Currency:           row.Currency,
		EventCount:         row.EventCount,
		ID:                 row.ID,
		Lines:              lines,
		PaidAt:             row.PaidAt.Time,
		PeriodEnd:          row.PeriodEnd.Time,
		PeriodStart:        row.PeriodStart.Time,
		ProviderInvoiceURL: row.ProviderInvoiceUrl.String,
		Status:             status,
		UsageCents:         row.UsageCents,
	}
	if row.TaxCents.Valid {
		inv.TaxCents = i64(row.TaxCents.Int64)
	}
	// The column keeps its date through a charge, when nothing is scheduled any more.
	if status == InvoiceOpen || status == InvoiceFailed {
		inv.NextAttemptAt = row.NextAttemptAt.Time
	}
	return inv, nil
}

// nextChargeAt is a dated invoice's attempt, else the charge of the next period to
// close — still the previous period's while that period has days no invoice has billed.
func (s *Service) nextChargeAt(
	ctx context.Context, orgID string, row dbread.GetOrgEntitlementRow, ent Entitlement, grace time.Duration,
) (time.Time, error) {
	read := dbread.New(s.pgW)
	next, err := read.GetNextBillingInvoiceAttempt(ctx, orgID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to read the next invoice charge", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return time.Time{}, err
	}
	if next.Valid {
		return next.Time, nil
	}
	billed, err := read.GetBillingInvoiceBilledTo(ctx, orgID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to read where billing has reached", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return time.Time{}, err
	}
	subs, _, err := s.subscriptionsOf(ctx, orgID)
	if err != nil {
		return time.Time{}, err
	}
	rec, orgCreate := recordFromRow(row), row.OrgCreateTime.Time
	prevStart, _ := coreusage.PeriodFor(ent.PeriodStart.Add(-time.Nanosecond), coreusage.AnchorDay(orgCreate, rec.AnchorDay))
	end := ent.PeriodEnd
	if len(billableSegments(orgCreate, rec, subs, period{prevStart, ent.PeriodStart}, billed.Time)) > 0 {
		end = ent.PeriodStart
	}
	return chargeAfter(end, grace), nil
}
