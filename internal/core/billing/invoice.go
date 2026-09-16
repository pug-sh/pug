package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
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

// InvoiceStatus is recorded, unlike the entitlement's: a charge is an event, not
// a comparison.
type InvoiceStatus string

const (
	InvoiceOpen     InvoiceStatus = "open"
	InvoiceWaived   InvoiceStatus = "waived"
	InvoiceDeferred InvoiceStatus = "deferred"
)

const (
	ActorInvoicePass = "invoice-pass"

	// ChargeNoticeDays is a placeholder: the days an open invoice can be seen, and
	// voided, before its first charge.
	ChargeNoticeDays = 3

	// Placeholders pinned to the provider's fixed 40¢ fee (§8.7). Under
	// DeferUnderCents a close is carried forward rather than charged; a sweep under
	// WaiveUnderCents is written off rather than charged at a loss, and must stay
	// above the provider's 50¢ card minimum; a balance spanning MaxDeferPeriods
	// periods is swept.
	DeferUnderCents = 500
	WaiveUnderCents = 100
	MaxDeferPeriods = 12

	// maxClosePeriods is how many due periods a pass looks back over, so a stalled
	// meter or pass catches up rather than losing a month.
	maxClosePeriods = 3
	// dropReportWindow bounds how long a period that fell out of the catch-up
	// window keeps failing the pass. It is lost either way; a day of red is enough
	// to be seen.
	dropReportWindow = 24 * time.Hour
)

// ErrSubscriptionUndecodable is a stored subscription whose status pug has no
// word for. Skipping it would bill the org as if it never had a mandate.
var ErrSubscriptionUndecodable = errors.New("billing: a subscription holds an unknown status")

// Pricing is the snapshot an invoice was priced on, so a later catalog edit
// cannot change what it says it charged. Exactly one field is set.
type Pricing struct {
	Card  *RateCard    `json:"card,omitempty"`
	Terms *CustomTerms `json:"terms,omitempty"`
}

// CloseReport is what one close step did and found.
type CloseReport struct {
	Closed int
	Waived int
	// Deferred is a close under DeferUnderCents, carried to a later one.
	Deferred int
	// Swept is a close that ended a deferred balance, charged or written off.
	Swept int
	// Held is a due period the meter has not finalized yet.
	Held int
	// Dropped is a due period that left the catch-up window unbilled.
	Dropped int
	// AwaitingCard is a deal's invoice closed with no mandate to charge (§19.15).
	AwaitingCard int
	// Unpriceable is a period whose slug no card answers to.
	Unpriceable int
	Undecodable int
}

// period is one anniversary window, [start, end).
type period struct{ start, end time.Time }

// segment is one billable window inside a period, and what prices it.
type segment struct {
	from, to time.Time
	ent      Entitlement
	mandate  bool
}

// ClosePeriods invoices every period that is due and safe to price. grace is the
// meter's trailing window: after period_end + grace its count is as final as the
// meter makes it. Postgres failures return; an org pug cannot read is counted.
func (s *Service) ClosePeriods(ctx context.Context, now time.Time, grace time.Duration) (CloseReport, error) {
	var r CloseReport
	if !s.billingEnabled {
		return r, nil
	}
	orgs, err := dbread.New(s.pgW).ListBillingInvoiceOrgs(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list the orgs to invoice", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return r, err
	}
	for _, org := range orgs {
		if err := ctx.Err(); err != nil {
			return r, err
		}
		if err := s.closeOrg(ctx, org.ID, org.CreateTime.Time, now, grace, &r); err != nil {
			if errors.Is(err, ErrSubscriptionUndecodable) {
				r.Undecodable++
				continue
			}
			return r, err
		}
	}
	return r, nil
}

func (s *Service) closeOrg(ctx context.Context, orgID string, orgCreate, now time.Time, grace time.Duration, r *CloseReport) error {
	rec, err := s.StoredRecord(ctx, orgID)
	if err != nil {
		return err
	}
	subs, err := s.subscriptionsOf(ctx, orgID)
	if err != nil {
		return err
	}
	read := dbread.New(s.pgW)
	billed, err := read.GetBillingInvoiceBilledTo(ctx, orgID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to read where billing has reached", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return err
	}
	billedTo := billed.Time
	stamp, err := read.GetLatestUsageComputedAt(ctx, orgID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.ErrorContext(ctx, "failed to read the usage stamp", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return err
	}

	periods := duePeriods(orgCreate, coreusage.AnchorDay(orgCreate, rec.AnchorDay), now, grace)
	if len(periods) > maxClosePeriods {
		lost := periods[maxClosePeriods]
		if len(billableSegments(orgCreate, rec, subs, lost, billedTo)) > 0 &&
			now.Before(periods[0].end.Add(grace).Add(dropReportWindow)) {
			r.Dropped++
			err := fmt.Errorf("billing: org %s period %s left the catch-up window unbilled",
				orgID, lost.start.Format(time.DateOnly))
			slog.ErrorContext(ctx, "a due period was never invoiced", slogx.Error(err), slog.String("org_id", orgID))
			telemetry.RecordError(ctx, err)
		}
		periods = periods[:maxClosePeriods]
	}

	for _, p := range slices.Backward(periods) {
		for _, seg := range billableSegments(orgCreate, rec, subs, p, billedTo) {
			// A stalled meter delays an invoice, never mis-bills one. Newer periods need
			// a later stamp still, so the org stops here.
			if !stamp.Valid || stamp.Time.Before(p.end.Add(grace)) {
				r.Held++
				slog.WarnContext(ctx, "usage is not final yet; holding the invoice",
					slog.String("org_id", orgID), slog.Time("period_start", p.start),
					slog.Time("usage_computed_at", stamp.Time))
				return nil
			}
			if _, ok := seg.ent.quote(0); !ok {
				r.Unpriceable++
				err := fmt.Errorf("billing: org %s cannot be priced on plan %q", orgID, seg.ent.Slug)
				slog.ErrorContext(ctx, "period cannot be priced; holding the invoice", slogx.Error(err),
					slog.String("org_id", orgID), slog.Time("period_start", p.start))
				telemetry.RecordError(ctx, err)
				return nil
			}
			inserted, err := s.closeSegment(ctx, orgID, p, seg, stamp.Time, now, r)
			if err != nil || !inserted {
				return err
			}
			billedTo = seg.to
		}
	}
	return nil
}

// duePeriods lists the org's periods whose end + grace has passed, newest first,
// one past the catch-up window so the caller can see what fell out of it.
func duePeriods(orgCreate time.Time, anchor int, now time.Time, grace time.Duration) []period {
	start, _ := coreusage.PeriodFor(now.Add(-grace), anchor)
	var out []period
	for range maxClosePeriods + 1 {
		end := start
		start, _ = coreusage.PeriodFor(end.Add(-time.Nanosecond), anchor)
		if !end.After(orgCreate) {
			break
		}
		out = append(out, period{start: start, end: end})
	}
	return out
}

// billableSegments clips a period to the days that can be billed, starting at
// billedTo so no day is billed twice. A deal bills from its terms, card or not; a
// card org bills only the days a mandate was live, one segment per mandate. Trial
// days are never billed.
func billableSegments(orgCreate time.Time, rec Record, subs []Subscription, p period, billedTo time.Time) []segment {
	at := p.end.Add(-time.Nanosecond)
	floor := maxTime(p.start, billedTo, coreusage.CeilDayUTC(trialEnd(orgCreate, rec)))

	if _, deal := rec.Terms(); deal && !contractLapsed(rec, at) {
		from := maxTime(floor, coreusage.FloorDayUTC(rec.TermsEffectiveAt))
		if !from.Before(p.end) {
			return nil
		}
		return []segment{{from: from, to: p.end, ent: Resolve(orgCreate, rec, nil, at, true), mandate: liveAt(subs, at)}}
	}

	mandates := slices.Clone(subs)
	slices.SortFunc(mandates, func(a, b Subscription) int { return a.CreateTime.Compare(b.CreateTime) })
	var out []segment
	for _, sub := range mandates {
		if !sub.OnDemand {
			continue
		}
		from := maxTime(floor, coreusage.FloorDayUTC(sub.CreateTime))
		to := p.end
		if ended := sub.endedAt(); !ended.IsZero() && ended.Before(to) {
			to = coreusage.FloorDayUTC(ended)
		}
		if !from.Before(to) {
			continue
		}
		// Priced as the mandate stood: a cancelled one still pins its card.
		live := sub
		live.Status = SubStatusActive
		out = append(out, segment{from: from, to: to, ent: Resolve(orgCreate, rec, &live, at, true), mandate: true})
		floor = to
	}
	return out
}

func (s *Service) closeSegment(
	ctx context.Context, orgID string, p period, seg segment, stamp, now time.Time, r *CloseReport,
) (bool, error) {
	events, err := dbread.New(s.pgW).SumUsageDaily(ctx, dbread.SumUsageDailyParams{
		OrgID: orgID, FromDay: postgres.NewDate(seg.from), ToDay: postgres.NewDate(seg.to),
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to sum usage for an invoice", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return false, err
	}
	quote, _ := seg.ent.quote(events)
	lines, err := json.Marshal(quote.Lines)
	if err != nil {
		slog.ErrorContext(ctx, "failed to encode the invoice lines", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return false, err
	}
	pricing, err := json.Marshal(Pricing{Card: seg.ent.Card, Terms: seg.ent.Terms})
	if err != nil {
		slog.ErrorContext(ctx, "failed to encode the invoice pricing", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return false, err
	}

	params := dbwrite.InsertBillingInvoiceParams{
		BilledFrom:      postgres.NewDate(seg.from),
		BilledTo:        postgres.NewDate(seg.to),
		Currency:        seg.ent.Currency,
		EventCount:      events,
		ID:              xid.New().String(),
		Lines:           lines,
		OrgID:           orgID,
		PeriodEnd:       postgres.NewTimestamptz(p.end),
		PeriodStart:     postgres.NewTimestamptz(p.start),
		PlanSlug:        seg.ent.Slug,
		Pricing:         pricing,
		UsageCents:      quote.TotalCents,
		UsageComputedAt: postgres.NewTimestamptz(stamp),
	}

	tx, err := s.begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	w := dbwrite.New(tx)
	carried, err := w.LockUncoveredDeferredBillingInvoices(ctx, orgID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to lock the deferred balance", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return false, err
	}
	var balance int64
	ids := make([]string, 0, len(carried))
	for _, c := range carried {
		balance += c.UsageCents
		ids = append(ids, c.ID)
	}
	sweep := len(carried) > 0 && periodsSpanned(carried[0].PeriodStart.Time, p.start) >= MaxDeferPeriods
	d := deferClose(quote.TotalCents, balance, sweep)
	params.Status = string(d.status)
	if d.carry {
		params.CarriedCents = balance
	}
	params.AmountCents = params.UsageCents + params.CarriedCents
	if d.status == InvoiceOpen {
		params.NextAttemptAt = postgres.NewTimestamptz(now.AddDate(0, 0, ChargeNoticeDays))
	}

	row, err := w.InsertBillingInvoice(ctx, params)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			slog.InfoContext(ctx, "another pass already closed this window",
				slog.String("org_id", orgID), slog.Time("billed_from", seg.from))
			return false, nil
		}
		slog.ErrorContext(ctx, "failed to insert an invoice", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return false, err
	}
	if err := appendInvoiceEvent(ctx, w, row.ID, "", d.status, ""); err != nil {
		return false, err
	}
	switch {
	case d.carry && len(ids) > 0:
		if _, err := w.CoverBillingInvoices(ctx, dbwrite.CoverBillingInvoicesParams{
			CoveredBy: postgres.NewOptionalText(row.ID), Ids: ids,
		}); err != nil {
			slog.ErrorContext(ctx, "failed to cover the deferred balance", slogx.Error(err), slog.String("org_id", orgID))
			telemetry.RecordError(ctx, err)
			return false, err
		}
	case d.waiveBalance && len(ids) > 0:
		if _, err := w.WaiveDeferredBillingInvoices(ctx, ids); err != nil {
			slog.ErrorContext(ctx, "failed to waive the deferred balance", slogx.Error(err), slog.String("org_id", orgID))
			telemetry.RecordError(ctx, err)
			return false, err
		}
		for _, id := range ids {
			if err := appendInvoiceEvent(ctx, w, id, InvoiceDeferred, InvoiceWaived, "swept by "+row.ID); err != nil {
				return false, err
			}
		}
	}
	if err := s.commit(ctx, tx, orgID); err != nil {
		return false, err
	}

	switch d.status {
	case InvoiceWaived:
		r.Waived++
	case InvoiceDeferred:
		r.Deferred++
	case InvoiceOpen:
		r.Closed++
		if seg.ent.Terms != nil && !seg.mandate {
			r.AwaitingCard++
		}
	}
	if sweep {
		r.Swept++
	}
	return true, nil
}

type deferral struct {
	status       InvoiceStatus
	carry        bool
	waiveBalance bool
}

// deferClose is §8.7's table: whether a close is charged, carried forward or
// written off, given its own usage and the balance earlier closes deferred.
func deferClose(usage, balance int64, sweep bool) deferral {
	total := usage + balance
	switch {
	case sweep && total >= WaiveUnderCents:
		return deferral{status: InvoiceOpen, carry: true}
	case sweep:
		return deferral{status: InvoiceWaived, waiveBalance: true}
	case usage == 0:
		return deferral{status: InvoiceWaived}
	case total < DeferUnderCents:
		return deferral{status: InvoiceDeferred}
	}
	return deferral{status: InvoiceOpen, carry: true}
}

// periodsSpanned counts the monthly periods from the one starting at first through
// the one starting at last, both included.
func periodsSpanned(first, last time.Time) int {
	first, last = first.UTC(), last.UTC()
	return (last.Year()-first.Year())*12 + int(last.Month()-first.Month()) + 1
}

func appendInvoiceEvent(ctx context.Context, w *dbwrite.Queries, invoiceID string, from, to InvoiceStatus, detail string) error {
	if err := w.InsertBillingInvoiceEvent(ctx, dbwrite.InsertBillingInvoiceEventParams{
		Actor: ActorInvoicePass, Detail: detail, FromStatus: string(from), ID: xid.New().String(),
		InvoiceID: invoiceID, ToStatus: string(to),
	}); err != nil {
		slog.ErrorContext(ctx, "failed to record an invoice event", slogx.Error(err), slog.String("invoice_id", invoiceID))
		telemetry.RecordError(ctx, err)
		return err
	}
	return nil
}

// subscriptionsOf is every mandate the org ever held, live or not: a close bills
// the days each one covered.
func (s *Service) subscriptionsOf(ctx context.Context, orgID string) ([]Subscription, error) {
	rows, err := dbread.New(s.pgW).ListBillingSubscriptionsByOrg(ctx, orgID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list the org's subscriptions", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return nil, err
	}
	out := make([]Subscription, 0, len(rows))
	for _, row := range rows {
		sub, ok := subscriptionFromRow(row)
		if !ok {
			err := fmt.Errorf("%w: org %s subscription %s holds %q", ErrSubscriptionUndecodable, orgID, row.ID, row.Status)
			slog.ErrorContext(ctx, "a subscription holds a status pug does not know", slogx.Error(err),
				slog.String("org_id", orgID))
			telemetry.RecordError(ctx, err)
			return nil, err
		}
		out = append(out, sub)
	}
	return out, nil
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

func liveAt(subs []Subscription, at time.Time) bool {
	for _, sub := range subs {
		if sub.OnDemand && !sub.CreateTime.After(at) {
			if ended := sub.endedAt(); ended.IsZero() || ended.After(at) {
				return true
			}
		}
	}
	return false
}

func maxTime(first time.Time, rest ...time.Time) time.Time {
	out := first
	for _, t := range rest {
		if t.After(out) {
			out = t
		}
	}
	return out
}
