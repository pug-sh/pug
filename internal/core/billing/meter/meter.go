package meter

import (
	"context"
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

// Service is the meter pass.
type Service struct {
	// usage is read from the replica: a lagging sum only delays a statement, and the
	// next tick states the rest.
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

// Report is what one pass did.
type Report struct {
	Orgs, Stated, Unchanged, Frozen, Resent, Failed int
}

type outcome int

const (
	outcomeUnchanged outcome = iota
	outcomeStated
	outcomeFrozen
)

// Run states every live subscription's usage once. One org's failure never stops
// the others; the error reports how many failed, so the CronJob goes red.
func (s *Service) Run(ctx context.Context, now time.Time) (Report, error) {
	var report Report
	subs, err := s.ledger.ListLiveBillingSubscriptionsForMeter(ctx, s.provider)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list the subscriptions to meter", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return report, err
	}
	for _, sub := range subs {
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
		case outcomeUnchanged:
			report.Unchanged++
		}
	}
	if report.Failed > 0 {
		return report, fmt.Errorf("meter: %d of %d orgs failed", report.Failed, report.Orgs)
	}
	return report, nil
}

// meterOrg states one org's current period. now is the tick's start, used as every
// statement's timestamp: it is before the period's end by the freeze rule, and the
// pass's 30-minute timeout keeps it inside the provider's one-hour window.
func (s *Service) meterOrg(ctx context.Context, sub dbread.BillingSubscription, now time.Time) (outcome, bool, error) {
	orgID := sub.OrgID
	periodStart, periodEnd := sub.CurrentPeriodStart.Time, sub.CurrentPeriodEnd.Time
	// The freeze rule: past the nominal renewal the provider is either processing it,
	// with a statement's period unmeasured, or holding the subscription. Wait for the
	// new period; the days keep counting and reach the provider through the carry.
	if !now.Before(periodEnd) {
		return outcomeFrozen, false, nil
	}

	row, err := readPeriod(ctx, s.ledger, orgID, periodStart)
	if err != nil {
		return 0, false, err
	}
	resent := false
	if row != nil && !row.Acked {
		// The last statement may never have landed: send it again, byte-identical.
		if err := s.ingest(ctx, orgID, *row, now); err != nil {
			return 0, false, err
		}
		if err := ack(ctx, s.w, orgID, *row); err != nil {
			return 0, false, err
		}
		row.Acked, resent = true, true
	}

	ent, err := s.entitlements.GetEntitlement(ctx, orgID, now)
	if err != nil {
		return 0, resent, err
	}
	// Absent only for a slug the catalog no longer knows: there is no layout to split
	// by, and guessing one would bill at rates nobody agreed to. An empty TierUpTo is
	// a plan of one unbounded tier, which splits fine.
	if ent.IncludedEvents == nil {
		err := fmt.Errorf("meter: org %s resolves to %q, which has no tiers to split by", orgID, ent.Slug)
		slog.ErrorContext(ctx, "cannot meter an org with no tiers", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return 0, resent, err
	}
	allowance := *ent.IncludedEvents

	prev, err := readPreviousPeriod(ctx, s.ledger, orgID, periodStart)
	if err != nil {
		return 0, resent, err
	}
	// The previous period's last ingest failed and the freeze kept it from being
	// re-sent, since a statement stamped now could land in this period and bill the
	// old count twice. It may still have landed, so the carry counts it as sent: up
	// to one tick's growth goes unbilled, the direction chosen. Said once, when this
	// period is first stated.
	if row == nil && prev != nil && !prev.Acked {
		slog.WarnContext(ctx, "the previous meter period ended with its last statement unacknowledged; up to one tick's growth may be unbilled",
			slog.String("org_id", orgID), slog.Time("previous_period_start", prev.Start))
	}
	var statedWin, prevWin *Window
	if row != nil {
		statedWin = &row.Window
	}
	if prev != nil {
		prevWin = &prev.Window
	}
	win := windowFor(periodStart, periodEnd, statedWin, prevWin)

	total, err := s.sum(ctx, orgID, win.Start, win.through(now))
	if err != nil {
		return 0, resent, err
	}
	own := Split(total, allowance, ent.TierUpTo)
	carried := make([]int64, len(own))
	if prev != nil && prev.Window.contiguousWith(win) {
		if carried, err = s.carryFrom(ctx, orgID, *prev, len(own)); err != nil {
			return 0, resent, err
		}
	}
	if row != nil {
		if len(row.Own) != len(own) {
			err := fmt.Errorf("meter: org %s's tier count changed mid-period, %d to %d", orgID, len(row.Own), len(own))
			slog.ErrorContext(ctx, "cannot restate a period under different tiers", slogx.Error(err), slog.String("org_id", orgID))
			telemetry.RecordError(ctx, err)
			return 0, resent, err
		}
		own, carried = maxEach(row.Own, own), maxEach(row.Carry, carried)
		if slices.Equal(own, row.Own) && slices.Equal(carried, row.Carry) {
			return outcomeUnchanged, resent, nil
		}
	}

	// The plan the period split by, not the one the org holds: a deal holds custom,
	// which names no layout, and carryFrom and the dashboard look the layout up.
	next := period{
		Start: periodStart, Window: win, PlanSlug: ent.TierPlanSlug, Allowance: allowance,
		Own: own, Carry: carried, CustomerID: sub.ProviderCustomerID,
		// Microseconds, what timestamptz stores: the ack matches on it exactly.
		StatedAt: now.UTC().Truncate(time.Microsecond),
	}
	if err := writeAhead(ctx, s.w, orgID, next); err != nil {
		return 0, resent, err
	}
	if err := s.ingest(ctx, orgID, next, now); err != nil {
		return 0, resent, err
	}
	if err := ack(ctx, s.w, orgID, next); err != nil {
		return 0, resent, err
	}
	return outcomeStated, resent, nil
}

// carryFrom re-splits a finished window under the terms it was stated under — its
// own plan's tiers and allowance — and returns what it still owes each tier.
func (s *Service) carryFrom(ctx context.Context, orgID string, prev period, tiers int) ([]int64, error) {
	layout, ok := entitlement.TiersFor(prev.PlanSlug)
	if !ok || len(layout)+1 != len(prev.Own) || len(prev.Own) != tiers {
		// A second tier layout is deliberately not handled yet. Carrying nothing is
		// the under-billing direction.
		slog.WarnContext(ctx, "the previous meter period was split under different tiers; carrying nothing",
			slog.String("org_id", orgID), slog.String("plan_slug", prev.PlanSlug))
		return make([]int64, tiers), nil
	}
	total, err := s.sum(ctx, orgID, prev.Window.Start, prev.Window.End)
	if err != nil {
		return nil, err
	}
	return carry(Split(total, prev.Allowance, layout), prev.Own), nil
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

func (s *Service) ingest(ctx context.Context, orgID string, p period, now time.Time) error {
	stated := p.stated()
	err := s.meter.IngestUsage(ctx, billing.UsageStatement{
		CustomerID: p.CustomerID, EventID: eventID(orgID, p.Start, stated), At: now, TierEvents: stated,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to state usage to the provider", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return err
	}
	return nil
}
