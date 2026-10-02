package meter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
)

// period is one ledger row: a provider period as pug last stated it.
type period struct {
	Start  time.Time
	Window Window
	// SummedThrough is where Window's days were summed to, as of the last tick that
	// saw the subscription live.
	SummedThrough time.Time
	// The catalog plan whose tiers the period was split by: a subscriber's own plan,
	// or a deal's base plan. TiersFor gives its layout back.
	PlanSlug   string
	Allowance  int64
	Own, Carry []int64
	CustomerID string
	SubID      string
	StatedAt   time.Time
	Acked      bool
}

func (p period) stated() []int64 { return sumEach(p.Own, p.Carry) }

// daysFor is the run of p's days that a period of subscription subID follows. Its
// whole window across a renewal of the same subscription, which was live
// throughout. Across a change of subscription only what was summed while p's was
// live: the rest of p's window is days the org no longer held it, which belong to
// the new subscription's own window or, in a gap, to none.
func (p period) daysFor(subID string) Window {
	if p.SubID == subID {
		return p.Window
	}
	return Window{Start: p.Window.Start, End: p.SummedThrough}
}

func periodFromRow(row dbread.BillingMeterPeriod) period {
	return period{
		Start:         row.PeriodStart.Time,
		Window:        Window{Start: row.WindowStart.Time, End: row.WindowEnd.Time},
		SummedThrough: row.SummedThrough.Time,
		PlanSlug:      row.PlanSlug,
		Allowance:     row.Allowance,
		Own:           row.OwnEvents,
		Carry:         row.CarryEvents,
		CustomerID:    row.ProviderCustomerID,
		SubID:         row.ProviderSubID,
		StatedAt:      row.StatedAt.Time,
		Acked:         row.Acked,
	}
}

// readPeriod returns nil for a period never stated, the ordinary case.
func readPeriod(ctx context.Context, q *dbread.Queries, orgID string, start time.Time) (*period, error) {
	row, err := q.GetBillingMeterPeriod(ctx, dbread.GetBillingMeterPeriodParams{
		OrgID: orgID, PeriodStart: postgres.NewTimestamptz(start),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		slog.ErrorContext(ctx, "failed to read the meter period", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return nil, err
	}
	p := periodFromRow(row)
	return &p, nil
}

// readPreviousPeriod returns the period whose days the period start of
// subscription subID follows, or nil for an org's first.
func readPreviousPeriod(ctx context.Context, q *dbread.Queries, orgID string, start time.Time, subID string) (*period, error) {
	row, err := q.GetPreviousBillingMeterPeriod(ctx, dbread.GetPreviousBillingMeterPeriodParams{
		OrgID: orgID, PeriodStart: postgres.NewTimestamptz(start), ProviderSubID: subID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		slog.ErrorContext(ctx, "failed to read the previous meter period", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return nil, err
	}
	p := periodFromRow(row)
	return &p, nil
}

func writeAhead(ctx context.Context, w *dbwrite.Queries, orgID string, p period) error {
	err := w.WriteAheadBillingMeterPeriod(ctx, dbwrite.WriteAheadBillingMeterPeriodParams{
		Allowance:          p.Allowance,
		CarryEvents:        p.Carry,
		OrgID:              orgID,
		OwnEvents:          p.Own,
		PeriodStart:        postgres.NewTimestamptz(p.Start),
		PlanSlug:           p.PlanSlug,
		ProviderCustomerID: p.CustomerID,
		ProviderSubID:      p.SubID,
		StatedAt:           postgres.NewTimestamptz(p.StatedAt),
		SummedThrough:      postgres.NewDate(p.SummedThrough),
		WindowEnd:          postgres.NewDate(p.Window.End),
		WindowStart:        postgres.NewDate(p.Window.Start),
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to write the meter statement ahead", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return err
	}
	return nil
}

// advance records that a tick stating nothing still saw the period's subscription
// live, through the day before through, and its window ending at end.
func advance(ctx context.Context, w *dbwrite.Queries, orgID string, start, end, through time.Time) error {
	err := w.AdvanceBillingMeterPeriod(ctx, dbwrite.AdvanceBillingMeterPeriodParams{
		OrgID: orgID, PeriodStart: postgres.NewTimestamptz(start), WindowEnd: postgres.NewDate(end),
		SummedThrough: postgres.NewDate(through),
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to advance the meter period", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return err
	}
	return nil
}

func ack(ctx context.Context, w *dbwrite.Queries, orgID string, p period) error {
	n, err := w.AckBillingMeterPeriod(ctx, dbwrite.AckBillingMeterPeriodParams{
		OrgID: orgID, PeriodStart: postgres.NewTimestamptz(p.Start), StatedAt: postgres.NewTimestamptz(p.StatedAt),
	})
	if err == nil && n == 0 {
		err = fmt.Errorf("meter: the statement for org %s changed before its ack", orgID)
	}
	if err != nil {
		slog.ErrorContext(ctx, "failed to acknowledge the meter statement", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return err
	}
	return nil
}

// Stated is what the dashboard shows: a period's per-tier counts as last stated to
// the provider, carry included. A running figure, not an invoice: the provider
// bills each tier the most it was told by the period's end, and a statement still
// unacknowledged may not have reached it.
type Stated struct {
	// The plan and allowance the tiers were split by, for their bounds (see
	// period.PlanSlug): the allowance the org holds now can differ.
	PlanSlug  string
	Allowance int64
	Tiers     []int64
	AsOf      time.Time
}

// Reader serves the ledger to the dashboard. Built over the write pool: the pass
// writes the row and a replica would show the previous hour.
type Reader struct{ q *dbread.Queries }

func NewReader(q *dbread.Queries) *Reader { return &Reader{q: q} }

// Stated reports false for a period nothing has been stated for yet.
func (r *Reader) Stated(ctx context.Context, orgID string, periodStart time.Time) (Stated, bool, error) {
	p, err := readPeriod(ctx, r.q, orgID, periodStart)
	if err != nil || p == nil {
		return Stated{}, false, err
	}
	return Stated{PlanSlug: p.PlanSlug, Allowance: p.Allowance, Tiers: p.stated(), AsOf: p.StatedAt}, true, nil
}
