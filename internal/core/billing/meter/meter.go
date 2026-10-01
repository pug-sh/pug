package meter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/core/billing/entitlement"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
)

// usageStaleAfter is how old the usage pass's newest verified count may be before
// the meter refuses to state from the day cells it writes. The usage CronJob is
// meant to run hourly (docs/architecture/usage.md §4), so this allows a couple of
// missed runs. Stating stale cells would not over-bill — each statement is a lower
// bound the next supersedes — but a usage pass that stopped would bill every
// subscriber the fee alone, and only the refusal says so.
const usageStaleAfter = 3 * time.Hour

// frozenTooLong is how long past its nominal end a period may wait for its renewal
// before the pass reports it as an error. Dodo renews about an hour late, so a day
// is a subscription held on a failed card, or a renewal pug never heard about.
const frozenTooLong = 24 * time.Hour

// ErrUsageStale is a pass that stated nothing because the usage counts it states
// from were never computed or have stopped being refreshed.
var ErrUsageStale = errors.New("meter: the usage counts are stale")

// Service is the meter pass.
type Service struct {
	// usage is read from the replica: a lagging sum only delays a statement, and the
	// next tick states the rest. Its freshness stamp is read there too, so the stamp
	// speaks for the cells actually summed.
	usage *dbread.Queries
	// ledger reads go to the primary, where the pass itself writes them.
	ledger       *dbread.Queries
	w            *dbwrite.Queries
	entitlements *entitlement.Service
	meter        billing.UsageMeter
	provider     string
}

// NewService builds the pass over one provider's meters; provider is the name its
// subscriptions are stored under.
func NewService(pgRO, pgW *pgxpool.Pool, entitlements *entitlement.Service, meter billing.UsageMeter, provider string) *Service {
	return &Service{
		usage: dbread.New(pgRO), ledger: dbread.New(pgW), w: dbwrite.New(pgW),
		entitlements: entitlements, meter: meter, provider: provider,
	}
}

// Report is what one pass did. Orgs is every live subscription the pass reached,
// and each lands in exactly one of Stated, Unchanged, Frozen, Superseded and
// Failed; Resent counts statements re-sent along the way.
type Report struct {
	Orgs, Stated, Unchanged, Frozen, Superseded, Resent, Failed int
}

type outcome int

const (
	outcomeUnchanged outcome = iota
	outcomeStated
	outcomeFrozen
	// The subscription listed when the pass started was no longer the org's when its
	// turn came: cancelled, replaced, renewed, or deleted with its org.
	outcomeSuperseded
)

// Run states every live subscription's usage once. One org's failure never stops
// the others; the error reports how many failed, so the CronJob goes red.
//
// The caller holds cron.JobBillingMeter's lock, as cmd/cron/billing-meter does: a
// statement is the max of what the pass computed and the ledger row it read, and
// the write-ahead overwrites that row, so an overlapping pass could lower it.
func (s *Service) Run(ctx context.Context, now time.Time) (Report, error) {
	var report Report
	subs, err := s.ledger.ListLiveBillingSubscriptionsForMeter(ctx, s.provider)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list the subscriptions to meter", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return report, err
	}
	if len(subs) == 0 {
		return report, nil
	}
	if err := s.checkUsageFresh(ctx, now); err != nil {
		return report, err
	}
	for _, sub := range subs {
		// Without this a pass past its deadline reports every org it had not reached
		// as a failed read, pointing the operator at Postgres.
		if err := ctx.Err(); err != nil {
			err = fmt.Errorf("meter: cut short after %d of %d orgs: %w", report.Orgs, len(subs), err)
			slog.ErrorContext(ctx, "billing meter pass was cut short", slogx.Error(err),
				slog.Int("orgs_done", report.Orgs), slog.Int("orgs", len(subs)))
			telemetry.RecordError(ctx, err)
			return report, err
		}
		report.Orgs++
		got, resent, err := s.meterOrg(ctx, sub, now)
		if resent {
			report.Resent++
		}
		if err != nil {
			report.Failed++ // logged where it was detected
			continue
		}
		switch got {
		case outcomeStated:
			report.Stated++
		case outcomeFrozen:
			report.Frozen++
		case outcomeSuperseded:
			report.Superseded++
		case outcomeUnchanged:
			report.Unchanged++
		}
	}
	if report.Failed > 0 {
		return report, fmt.Errorf("meter: %d of %d orgs failed", report.Failed, report.Orgs)
	}
	return report, nil
}

// checkUsageFresh refuses the pass when the usage pass, whose day cells every
// statement is summed from, has never run or has stopped verifying counts.
func (s *Service) checkUsageFresh(ctx context.Context, now time.Time) error {
	at, err := s.usage.GetUsageComputedAt(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to read when usage was last metered", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}
	switch age := now.Sub(at.Time); {
	case !at.Valid:
		err = fmt.Errorf("%w: pug cron usage has never metered this deployment", ErrUsageStale)
	case age > usageStaleAfter:
		err = fmt.Errorf("%w: pug cron usage last metered at %s, %s ago, past the %s bound",
			ErrUsageStale, at.Time.UTC().Format(time.RFC3339), age.Round(time.Minute), usageStaleAfter)
	default:
		return nil
	}
	slog.ErrorContext(ctx, "refusing to state usage from stale counts", slogx.Error(err))
	telemetry.RecordError(ctx, err)
	return err
}

// meterOrg states one org's current period. now is the tick's start, used as every
// statement's timestamp: it is before the period's end by the freeze rule, and the
// pass's 30-minute timeout keeps it inside the provider's one-hour window.
func (s *Service) meterOrg(ctx context.Context, sub dbread.BillingSubscription, now time.Time) (outcome, bool, error) {
	orgID := sub.OrgID
	// Listed rather than filtered out of the work list, where it would never be
	// metered while the pass exited 0.
	if !sub.CurrentPeriodStart.Valid || !sub.CurrentPeriodEnd.Valid {
		err := fmt.Errorf("meter: org %s's live subscription %s has no period to state", orgID, sub.ProviderSubID)
		slog.ErrorContext(ctx, "cannot meter a subscription the provider has reported no period for", slogx.Error(err),
			slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return 0, false, err
	}
	periodStart, periodEnd := sub.CurrentPeriodStart.Time, sub.CurrentPeriodEnd.Time
	// The freeze rule: past the nominal renewal the provider is either processing it,
	// with a statement's period unmeasured, or holding the subscription. Wait for the
	// new period; the days keep counting and reach the provider through the carry.
	if !now.Before(periodEnd) {
		logFrozen(ctx, orgID, periodEnd, now)
		return outcomeFrozen, false, nil
	}

	row, err := readPeriod(ctx, s.ledger, orgID, periodStart)
	if err != nil {
		return 0, false, err
	}
	resent := false
	if row != nil && !row.Acked {
		// The last statement may never have landed: send it again — the same counts
		// and event id, stamped with this tick.
		if err := s.ingest(ctx, orgID, *row, now, true); err != nil {
			return 0, false, err
		}
		if err := ack(ctx, s.w, orgID, *row); err != nil {
			return 0, false, err
		}
		row.Acked, resent = true, true
	}

	ent, err := s.entitlements.GetEntitlement(ctx, orgID, now)
	if errors.Is(err, entitlement.ErrOrgNotFound) {
		// Deleted mid-pass, its subscription and ledger with it. GetEntitlement
		// returns this one unlogged.
		slog.WarnContext(ctx, "org was deleted mid-pass; nothing to meter", slog.String("org_id", orgID))
		return outcomeSuperseded, resent, nil
	}
	if err != nil {
		return 0, resent, err
	}
	// The subscription was listed when the pass started and the entitlement is read
	// now. A cancellation, cutover or renewal in between leaves terms that are not
	// this period's — a free org's, or another subscription's sent to this one's
	// customer. The next tick meters whatever is live then.
	if !ent.SubStatus.Live() || ent.ProviderSubID != sub.ProviderSubID || !ent.SubPeriodStart.Equal(periodStart) {
		slog.InfoContext(ctx, "org's subscription changed mid-pass; leaving it to the next tick",
			slog.String("org_id", orgID), slog.String("provider_sub_id", sub.ProviderSubID))
		return outcomeSuperseded, resent, nil
	}
	// No layout: a live subscription on a plan the catalog no longer knows, or a deal
	// pinned to one. Guessing a split would bill at rates nobody agreed to.
	if ent.TierPlanSlug == "" || ent.IncludedEvents == nil {
		err := fmt.Errorf("meter: org %s resolves to %q, which has no tiers to split by", orgID, ent.Slug)
		slog.ErrorContext(ctx, "cannot meter an org with no tiers", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return 0, resent, err
	}
	allowance := *ent.IncludedEvents
	tiers := len(ent.TierUpTo) + 1
	// A period restates only under the plan it was first split by: another plan's
	// tiers cover other bounds, so the max of the two splits could state more events
	// than were sent, and a max meter never comes back down. The allowance may move —
	// a deal re-set mid-period — and the max then keeps the larger count, as it does
	// over a recount that went down.
	if row != nil && (row.PlanSlug != ent.TierPlanSlug || len(row.Own) != tiers) {
		err := fmt.Errorf("meter: org %s's period was split by %s and now resolves to %s; it cannot be restated",
			orgID, row.PlanSlug, ent.TierPlanSlug)
		slog.ErrorContext(ctx, "cannot restate a period under different tiers", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return 0, resent, err
	}

	prev, err := readPreviousPeriod(ctx, s.ledger, orgID, periodStart, sub.ProviderSubID)
	if err != nil {
		return 0, resent, err
	}
	first := row == nil
	// The previous period's last ingest failed and the freeze kept it from being
	// re-sent, since a statement stamped now could land in this period and bill the
	// old count twice. It may still have landed, so the carry counts it as sent: up
	// to one tick's growth goes unbilled, the direction chosen. Said once, when this
	// period is first stated.
	if first && prev != nil && !prev.Acked {
		slog.WarnContext(ctx, "the previous meter period ended with its last statement unacknowledged; up to one tick's growth may be unbilled",
			slog.String("org_id", orgID), slog.Time("previous_period_start", prev.Start))
	}
	var statedWin, prevDays *Window
	if row != nil {
		statedWin = &row.Window
	}
	if prev != nil {
		days := prev.daysFor(sub.ProviderSubID)
		prevDays = &days
	}
	win := windowFor(periodStart, periodEnd, statedWin, prevDays)
	through := win.through(now)

	total, err := s.sum(ctx, orgID, win.Start, through)
	if err != nil {
		return 0, resent, err
	}
	own := Split(total, allowance, ent.TierUpTo)
	carried := make([]int64, tiers)
	switch {
	case prev == nil:
	case prevDays.contiguousWith(win):
		if carried, err = s.carryFrom(ctx, orgID, *prev, *prevDays, ent, first); err != nil {
			return 0, resent, err
		}
	case first:
		// Whatever the previous window still owed is dropped: by design across a gap,
		// when the org held no subscription, and the under-billing direction always.
		// Said once, when the window opens.
		slog.WarnContext(ctx, "the org's previous meter window does not meet this one; nothing carries",
			slog.String("org_id", orgID), slog.Time("previous_end", prevDays.End), slog.Time("window_start", win.Start))
	}
	if row != nil {
		own, carried = maxEach(row.Own, own), maxEach(row.Carry, carried)
		if slices.Equal(own, row.Own) && slices.Equal(carried, row.Carry) {
			// Nothing to state, but the tick saw the subscription live, which a change
			// of subscription later reads to know which days were this one's.
			if err := advance(ctx, s.w, orgID, periodStart, through); err != nil {
				return 0, resent, err
			}
			return outcomeUnchanged, resent, nil
		}
	}

	// The plan the period split by, not the one the org holds: a deal holds custom,
	// which names no layout, and carryFrom and the dashboard look the layout up.
	next := period{
		Start: periodStart, Window: win, SummedThrough: through, PlanSlug: ent.TierPlanSlug, Allowance: allowance,
		Own: own, Carry: carried, CustomerID: sub.ProviderCustomerID, SubID: sub.ProviderSubID,
		// Microseconds, what timestamptz stores: the ack matches on it exactly.
		StatedAt: now.UTC().Truncate(time.Microsecond),
	}
	if err := writeAhead(ctx, s.w, orgID, next); err != nil {
		return 0, resent, err
	}
	if err := s.ingest(ctx, orgID, next, now, false); err != nil {
		return 0, resent, err
	}
	if err := ack(ctx, s.w, orgID, next); err != nil {
		return 0, resent, err
	}
	return outcomeStated, resent, nil
}

// carryFrom re-splits the days of prev a period follows under the terms prev was
// stated under — its plan's tiers and its allowance — and returns what they still
// owe each tier. Only within one plan: a carry lands in the same tier of the current
// product, and another plan's tier covers other bounds. Carrying nothing is the
// under-billing direction, reported once, when the window opens.
func (s *Service) carryFrom(ctx context.Context, orgID string, prev period, days Window, ent entitlement.Entitlement, first bool) ([]int64, error) {
	tiers := len(ent.TierUpTo) + 1
	if prev.PlanSlug != ent.TierPlanSlug || len(prev.Own) != tiers {
		if first {
			err := fmt.Errorf("meter: org %s's previous period was split by %s and this one by %s; its shortfall is not carried",
				orgID, prev.PlanSlug, ent.TierPlanSlug)
			slog.ErrorContext(ctx, "the previous meter period was split under different tiers; carrying nothing",
				slogx.Error(err), slog.String("org_id", orgID))
			telemetry.RecordError(ctx, err)
		}
		return make([]int64, tiers), nil
	}
	total, err := s.sum(ctx, orgID, days.Start, days.End)
	if err != nil {
		return nil, err
	}
	return carry(Split(total, prev.Allowance, ent.TierUpTo), prev.Own), nil
}

// logFrozen names the org waiting on a renewal, and escalates once the wait is past
// what a late renewal explains: a period that never advances is never stated again,
// and its days reach the provider only once one does.
func logFrozen(ctx context.Context, orgID string, periodEnd, now time.Time) {
	waited := now.Sub(periodEnd)
	if waited < frozenTooLong {
		slog.InfoContext(ctx, "waiting for the org's renewal to be reported",
			slog.String("org_id", orgID), slog.Time("period_end", periodEnd), slog.Duration("waited", waited))
		return
	}
	err := fmt.Errorf("meter: org %s's period ended %s ago and no renewal has been reported", orgID, waited.Round(time.Minute))
	slog.ErrorContext(ctx, "an org's billing period has not advanced", slogx.Error(err),
		slog.String("org_id", orgID), slog.Time("period_end", periodEnd), slog.Duration("waited", waited))
	telemetry.RecordError(ctx, err)
}

func (s *Service) sum(ctx context.Context, orgID string, from, to time.Time) (int64, error) {
	if !from.Before(to) {
		return 0, nil
	}
	n, err := s.usage.SumOrgUsageDaily(ctx, dbread.SumOrgUsageDailyParams{
		OrgID: orgID, FromDay: postgres.NewDate(from), ToDay: postgres.NewDate(to),
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to sum the org's usage", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return 0, err
	}
	return n, nil
}

// ingest states p's counts and logs the statement: the ledger keeps only a period's
// latest, so the log is what traces a disputed invoice from the provider's event id
// back to the org and period it came from.
func (s *Service) ingest(ctx context.Context, orgID string, p period, now time.Time, resend bool) error {
	stated := p.stated()
	id := eventID(orgID, p.Start, stated)
	err := s.meter.IngestUsage(ctx, billing.UsageStatement{
		CustomerID: p.CustomerID, EventID: id, At: now, TierEvents: stated,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to state usage to the provider", slogx.Error(err),
			slog.String("org_id", orgID), slog.String("event_id", id))
		telemetry.RecordError(ctx, err)
		return err
	}
	slog.InfoContext(ctx, "stated usage to the provider", slog.String("org_id", orgID),
		slog.Time("period_start", p.Start), slog.String("event_id", id), slog.Any("tiers", stated),
		slog.Bool("resend", resend))
	return nil
}
