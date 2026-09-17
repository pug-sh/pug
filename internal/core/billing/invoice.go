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
	InvoiceOpen          InvoiceStatus = "open"
	InvoiceWaived        InvoiceStatus = "waived"
	InvoiceDeferred      InvoiceStatus = "deferred"
	InvoiceCharging      InvoiceStatus = "charging"
	InvoiceCharged       InvoiceStatus = "charged"
	InvoicePaid          InvoiceStatus = "paid"
	InvoiceRefunded      InvoiceStatus = "refunded"
	InvoiceFailed        InvoiceStatus = "failed"
	InvoiceUncollectible InvoiceStatus = "uncollectible"
	InvoiceVoid          InvoiceStatus = "void"
)

var invoiceStatuses = []InvoiceStatus{
	InvoiceOpen, InvoiceWaived, InvoiceDeferred, InvoiceCharging, InvoiceCharged,
	InvoicePaid, InvoiceRefunded, InvoiceFailed, InvoiceUncollectible, InvoiceVoid,
}

// AllInvoiceStatuses is every status this build knows, to assert against the column's
// own check constraint and the enum it maps to.
func AllInvoiceStatuses() []InvoiceStatus { return slices.Clone(invoiceStatuses) }

// ParseInvoiceStatus narrows a stored word back to the vocabulary. A newer migration's
// status can reach an older binary mid-deploy; false is that row.
func ParseInvoiceStatus(v string) (InvoiceStatus, bool) {
	s := InvoiceStatus(v)
	return s, slices.Contains(invoiceStatuses, s)
}

const (
	ActorInvoicePass   = "invoice-pass"
	ActorReconcilePass = "reconcile-pass"

	// ChargeNoticeDays is a placeholder: the days an open invoice can be seen
	// before its first charge.
	ChargeNoticeDays = 3

	// Grace is the meter's trailing window as a duration. Derived once so the date a
	// dashboard predicts and the date the pass charges on cannot drift apart.
	Grace = coreusage.RescanDays * 24 * time.Hour

	// Placeholders pinned to the provider's fixed 40¢ fee (§8.7).
	DeferUnderCents = 500
	// WaiveUnderCents must stay at or above the provider's 50¢ card minimum.
	WaiveUnderCents = 100
	MaxDeferPeriods = 12

	// maxClosePeriods is how many due periods a pass closes; an older one still
	// unbilled is written off.
	maxClosePeriods = 3
)

// chargeAfter is when a period ending at end is charged: the meter's grace, then the
// invoice's notice window.
func chargeAfter(end time.Time, grace time.Duration) time.Time {
	return end.Add(grace).AddDate(0, 0, ChargeNoticeDays)
}

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
	// Held (the meter is not final) and Unpriceable (no card answers to the slug)
	// count orgs stopped at a period; their later periods wait behind it.
	Held        int
	Unpriceable int
	// Dropped is a period written off because it left the catch-up window unbilled.
	Dropped int
	// AwaitingCard is a deal's invoice closed with no mandate live at its end (§19.15).
	AwaitingCard int
}

// period is one anniversary window, [start, end).
type period struct{ start, end time.Time }

// closing is a period's days up to `to`, as one pass closes them.
type closing struct {
	period
	to       time.Time
	kind     closeKind
	chargeAt time.Time
}

type window struct{ from, to time.Time }

// segment is the billable days inside a period that one invoice prices.
type segment struct {
	days    []window
	ent     Entitlement
	mandate bool
}

func (s segment) from() time.Time { return s.days[0].from }
func (s segment) to() time.Time   { return s.days[len(s.days)-1].to }

type closeKind int

const (
	closeDue closeKind = iota
	// closeFinal sweeps: with no live mandate and no deal in force, or while a
	// mandate is going, every close does, older periods a catch-up closes included.
	closeFinal
	// closeDropped writes off a period that left the catch-up window.
	closeDropped
)

// ClosePeriods invoices every period that is due and safe to price. grace is the
// meter's trailing window: after period_end + grace its count is as final as the
// meter makes it. A Postgres failure stops only its own org, and the first is
// returned; Held, Dropped and Unpriceable are findings for the caller, not errors.
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
	var firstErr error
	for _, org := range orgs {
		if err := ctx.Err(); err != nil {
			return r, err
		}
		err := s.closeOrg(ctx, org.ID, org.CreateTime.Time, now, grace, time.Time{}, ActorInvoicePass, &r)
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return r, firstErr
}

// closeOrg invoices the org's due periods. cutoff is the last instant a going mandate can
// be charged, read off a scheduled cancellation when zero; while set, closes sweep and charge
// by it, and the days finalized before it close early.
func (s *Service) closeOrg(
	ctx context.Context, orgID string, orgCreate, now time.Time, grace time.Duration, cutoff time.Time, actor string,
	r *CloseReport,
) error {
	rec, err := s.StoredRecord(ctx, orgID)
	if err != nil {
		return err
	}
	subs, _, err := s.subscriptionsOf(ctx, orgID)
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

	kind, chargeAt := closeDue, now.AddDate(0, 0, ChargeNoticeDays)
	if _, deal := rec.Terms(); !liveAt(subs, now) && (!deal || contractLapsed(rec, now)) {
		kind = closeFinal
	}
	if cutoff.IsZero() {
		cutoff = scheduledCutoff(subs)
	}
	if !cutoff.IsZero() {
		by := maxTime(now, cutoff)
		if err := s.chargeBy(ctx, orgID, by); err != nil {
			return err
		}
		kind, chargeAt = closeFinal, minTime(chargeAt, by)
	}
	anchor := coreusage.AnchorDay(orgCreate, rec.AnchorDay)
	var closes []closing
	for i, p := range slices.Backward(duePeriods(anchor, now, grace, maxTime(orgCreate, billedTo))) {
		c := closing{period: p, to: p.end, kind: kind, chargeAt: chargeAt}
		if i >= maxClosePeriods {
			c.kind = closeDropped
		}
		closes = append(closes, c)
	}
	if !cutoff.IsZero() {
		// The days the meter has finalized before the cutoff, unless their period closes in
		// time on its own; never a deal's, which waits for a card and would pay its fee twice.
		through := coreusage.FloorDayUTC(cutoff.Add(-grace))
		start, end := coreusage.PeriodFor(through.Add(-time.Nanosecond), anchor)
		_, deal := rec.Terms()
		if !now.Before(through.Add(grace)) && through.Before(end) && (!deal || contractLapsed(rec, through)) {
			closes = append(closes, closing{period: period{start, end}, to: through, kind: closeFinal, chargeAt: chargeAt})
		}
	}
	for _, c := range closes {
		for _, seg := range billableSegments(orgCreate, rec, subs, period{c.start, c.to}, billedTo) {
			// A stalled meter delays an invoice, never mis-bills one. Newer periods need
			// a later stamp still, so the org stops here.
			if !stamp.Valid || stamp.Time.Before(c.to.Add(grace)) {
				r.Held++
				slog.WarnContext(ctx, "usage is not final yet; holding the invoice",
					slog.String("org_id", orgID), slog.Time("period_start", c.start),
					slog.Time("usage_computed_at", stamp.Time))
				return nil
			}
			inserted, err := s.closeSegment(ctx, orgID, c, seg, stamp.Time, actor, r)
			if err != nil || !inserted {
				return err
			}
		}
	}
	return nil
}

// duePeriods lists the periods ending after since whose end + grace has passed,
// newest first.
func duePeriods(anchor int, now time.Time, grace time.Duration, since time.Time) []period {
	var out []period
	end, _ := coreusage.PeriodFor(now.Add(-grace), anchor)
	for end.After(since) {
		start, _ := coreusage.PeriodFor(end.Add(-time.Nanosecond), anchor)
		out = append(out, period{start: start, end: end})
		end = start
	}
	return out
}

// billableSegments clips a period to the days that can be billed, starting at
// billedTo so no day is billed twice. A deal bills its own days, card or not; the
// days either side of it bill only while a mandate was live. Trial days are never
// billed.
func billableSegments(orgCreate time.Time, rec Record, subs []Subscription, p period, billedTo time.Time) []segment {
	floor := maxTime(p.start, billedTo, coreusage.CeilDayUTC(trialEnd(orgCreate, rec)))
	if _, deal := rec.Terms(); !deal {
		return cardSegments(orgCreate, rec, subs, floor, p.end)
	}

	dealFrom := maxTime(floor, coreusage.FloorDayUTC(rec.TermsEffectiveAt))
	dealTo := p.end
	if !rec.ContractEndsAt.IsZero() && rec.ContractEndsAt.Before(p.end) {
		dealTo = maxTime(dealFrom, coreusage.FloorDayUTC(rec.ContractEndsAt))
	}
	// Cleared, or Resolve would price the card days on the deal with its allowance.
	noDeal := rec
	noDeal.PlanSlug, noDeal.IncludedEventsOverride = "", 0

	out := cardSegments(orgCreate, noDeal, subs, floor, minTime(dealFrom, p.end))
	if dealFrom.Before(dealTo) {
		at := dealTo.Add(-time.Nanosecond)
		out = append(out, segment{
			days: []window{{dealFrom, dealTo}}, ent: Resolve(orgCreate, rec, nil, at, true), mandate: liveAt(subs, at),
		})
	}
	return append(out, cardSegments(orgCreate, noDeal, subs, dealTo, p.end)...)
}

// cardSegments is the days in [from, to) a mandate was live, as one segment so a
// change of card adds no allowance. It is priced on the latest mandate: a
// cancelled one still pins its card.
func cardSegments(orgCreate time.Time, rec Record, subs []Subscription, from, to time.Time) []segment {
	mandates := slices.Clone(subs)
	slices.SortFunc(mandates, func(a, b Subscription) int { return a.CreateTime.Compare(b.CreateTime) })
	var seg segment
	for _, sub := range mandates {
		if !sub.OnDemand {
			continue
		}
		start := maxTime(from, coreusage.FloorDayUTC(sub.CreateTime))
		end := to
		if ended := sub.endedAt(); !ended.IsZero() && ended.Before(end) {
			end = coreusage.FloorDayUTC(ended)
		}
		if !start.Before(end) {
			continue
		}
		seg.days = append(seg.days, window{start, end})
		from = end
		live := sub
		live.Status = SubStatusActive
		seg.ent = Resolve(orgCreate, rec, &live, to.Add(-time.Nanosecond), true)
		seg.mandate = true
	}
	if len(seg.days) == 0 {
		return nil
	}
	return []segment{seg}
}

func (s *Service) closeSegment(
	ctx context.Context, orgID string, c closing, seg segment, stamp time.Time, actor string, r *CloseReport,
) (bool, error) {
	var events int64
	for _, d := range seg.days {
		n, err := dbread.New(s.pgW).SumUsageDaily(ctx, dbread.SumUsageDailyParams{
			OrgID: orgID, FromDay: postgres.NewDate(d.from), ToDay: postgres.NewDate(d.to),
		})
		if err != nil {
			slog.ErrorContext(ctx, "failed to sum usage for an invoice", slogx.Error(err), slog.String("org_id", orgID))
			telemetry.RecordError(ctx, err)
			return false, err
		}
		events += n
	}
	quote, ok := seg.ent.quote(events)
	if !ok {
		r.Unpriceable++
		err := fmt.Errorf("billing: org %s cannot be priced on plan %q", orgID, seg.ent.Slug)
		slog.ErrorContext(ctx, "period cannot be priced; holding the invoice", slogx.Error(err),
			slog.String("org_id", orgID), slog.Time("period_start", c.start))
		telemetry.RecordError(ctx, err)
		return false, nil
	}
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
		BilledFrom:      postgres.NewDate(seg.from()),
		BilledTo:        postgres.NewDate(seg.to()),
		Currency:        seg.ent.Currency,
		EventCount:      events,
		ID:              xid.New().String(),
		Lines:           lines,
		OrgID:           orgID,
		PeriodEnd:       postgres.NewTimestamptz(c.end),
		PeriodStart:     postgres.NewTimestamptz(c.start),
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
	for _, deferred := range carried {
		balance += deferred.UsageCents
		ids = append(ids, deferred.ID)
	}
	sweep := c.kind == closeFinal ||
		len(carried) > 0 && periodsSpanned(carried[0].PeriodStart.Time, c.start) >= MaxDeferPeriods
	d := deferClose(quote.TotalCents, balance, sweep)
	detail := ""
	if c.kind == closeDropped {
		sweep, d, detail = false, deferral{status: InvoiceWaived}, "dropped"
	}
	params.Status = string(d.status)
	if d.status == InvoiceOpen {
		params.CarriedCents = balance
		params.NextAttemptAt = postgres.NewTimestamptz(c.chargeAt)
	}
	params.AmountCents = params.UsageCents + params.CarriedCents

	row, err := w.InsertBillingInvoice(ctx, params)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			slog.WarnContext(ctx, "another pass already closed this window",
				slog.String("org_id", orgID), slog.Time("billed_from", seg.from()))
			return false, nil
		}
		slog.ErrorContext(ctx, "failed to insert an invoice", slogx.Error(err),
			slog.String("org_id", orgID), slog.Time("billed_from", seg.from()))
		telemetry.RecordError(ctx, err)
		return false, err
	}
	if err := appendInvoiceEvent(ctx, w, orgID, row.ID, actor, "", d.status, detail); err != nil {
		return false, err
	}
	switch {
	case d.status == InvoiceOpen && len(ids) > 0:
		n, err := w.CoverBillingInvoices(ctx, dbwrite.CoverBillingInvoicesParams{
			CoveredBy: postgres.NewOptionalText(row.ID), Ids: ids,
		})
		if err == nil && n != int64(len(ids)) {
			err = fmt.Errorf("billing: covered %d of %d deferred invoices", n, len(ids))
		}
		if err != nil {
			slog.ErrorContext(ctx, "failed to cover the deferred balance", slogx.Error(err), slog.String("org_id", orgID))
			telemetry.RecordError(ctx, err)
			return false, err
		}
	case d.waiveBalance && len(ids) > 0:
		n, err := w.WaiveDeferredBillingInvoices(ctx, ids)
		if err == nil && n != int64(len(ids)) {
			err = fmt.Errorf("billing: waived %d of %d deferred invoices", n, len(ids))
		}
		if err != nil {
			slog.ErrorContext(ctx, "failed to waive the deferred balance", slogx.Error(err), slog.String("org_id", orgID))
			telemetry.RecordError(ctx, err)
			return false, err
		}
		for _, id := range ids {
			if err := appendInvoiceEvent(ctx, w, orgID, id, actor, InvoiceDeferred, InvoiceWaived, "swept by "+row.ID); err != nil {
				return false, err
			}
		}
	}
	if err := s.commit(ctx, tx, orgID); err != nil {
		return false, err
	}

	switch {
	case c.kind == closeDropped:
		r.Dropped++
		err := fmt.Errorf("billing: org %s wrote off [%s, %s), which left the catch-up window unbilled",
			orgID, seg.from().Format(time.DateOnly), seg.to().Format(time.DateOnly))
		slog.ErrorContext(ctx, "a due period was never invoiced", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
	case d.status == InvoiceWaived:
		r.Waived++
	case d.status == InvoiceDeferred:
		r.Deferred++
	case d.status == InvoiceOpen:
		r.Closed++
		if seg.ent.Terms != nil && !seg.mandate {
			r.AwaitingCard++
		}
	}
	if sweep && len(ids) > 0 {
		r.Swept++
	}
	return true, nil
}

type deferral struct {
	status       InvoiceStatus
	waiveBalance bool
}

// deferClose is §8.7's table: whether a close is charged, carried forward or
// written off, given its own usage and the balance earlier closes deferred. An
// open close carries the balance.
func deferClose(usage, balance int64, sweep bool) deferral {
	total := usage + balance
	switch {
	case sweep && total >= WaiveUnderCents:
		return deferral{status: InvoiceOpen}
	case sweep:
		return deferral{status: InvoiceWaived, waiveBalance: true}
	case usage == 0:
		return deferral{status: InvoiceWaived}
	case total < DeferUnderCents:
		return deferral{status: InvoiceDeferred}
	}
	return deferral{status: InvoiceOpen}
}

// periodsSpanned counts the monthly periods from the one starting at first through
// the one starting at last, both included.
func periodsSpanned(first, last time.Time) int {
	first, last = first.UTC(), last.UTC()
	return (last.Year()-first.Year())*12 + int(last.Month()-first.Month()) + 1
}

func appendInvoiceEvent(
	ctx context.Context, w *dbwrite.Queries, orgID, invoiceID, actor string, from, to InvoiceStatus, detail string,
) error {
	if err := w.InsertBillingInvoiceEvent(ctx, dbwrite.InsertBillingInvoiceEventParams{
		Actor: actor, Detail: detail, FromStatus: string(from), ID: xid.New().String(),
		InvoiceID: invoiceID, ToStatus: string(to),
	}); err != nil {
		slog.ErrorContext(ctx, "failed to record an invoice event", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("invoice_id", invoiceID))
		telemetry.RecordError(ctx, err)
		return err
	}
	return nil
}

// subscriptionsOf is every mandate the org ever held, live or not: a close bills
// the days each one covered. One with an unrecognized status covered none, and the
// on-demand ones are counted, since a charge cannot tell whether they are live.
func (s *Service) subscriptionsOf(ctx context.Context, orgID string) ([]Subscription, int, error) {
	rows, err := dbread.New(s.pgW).ListBillingSubscriptionsByOrg(ctx, orgID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list the org's subscriptions", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return nil, 0, err
	}
	out := make([]Subscription, 0, len(rows))
	unnamed := 0
	for _, row := range rows {
		sub, ok := subscriptionFromRow(row)
		switch {
		case ok:
			out = append(out, sub)
		case row.OnDemand:
			unnamed++
		}
	}
	return out, unnamed, nil
}

// endedAt is when a mandate stopped, zero while live. Every write stamps an ended
// row's date, so a missing one bills nothing rather than forever.
func (s Subscription) endedAt() time.Time {
	switch {
	case s.Status.Live():
		return time.Time{}
	case s.EndedAt.IsZero():
		return s.CreateTime
	}
	return s.EndedAt
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

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
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
