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
	Start      time.Time
	Window     Window
	PlanSlug   string
	Allowance  int64
	Own, Carry []int64
	CustomerID string
	StatedAt   time.Time
	Acked      bool
}

func (p period) stated() []int64 { return sumEach(p.Own, p.Carry) }

func periodFromRow(row dbread.BillingMeterPeriod) period {
	return period{
		Start:      row.PeriodStart.Time,
		Window:     Window{Start: row.WindowStart.Time, End: row.WindowEnd.Time},
		PlanSlug:   row.PlanSlug,
		Allowance:  row.Allowance,
		Own:        row.OwnEvents,
		Carry:      row.CarryEvents,
		CustomerID: row.ProviderCustomerID,
		StatedAt:   row.StatedAt.Time,
		Acked:      row.Acked,
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

func readPreviousPeriod(ctx context.Context, q *dbread.Queries, orgID string, start time.Time) (*period, error) {
	row, err := q.GetPreviousBillingMeterPeriod(ctx, dbread.GetPreviousBillingMeterPeriodParams{
		OrgID: orgID, PeriodStart: postgres.NewTimestamptz(start),
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
		StatedAt:           postgres.NewTimestamptz(p.StatedAt),
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
// the provider, carry included — exactly what it will bill.
type Stated struct {
	PlanSlug string
	Tiers    []int64
	AsOf     time.Time
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
	return Stated{PlanSlug: p.PlanSlug, Tiers: p.stated(), AsOf: p.StatedAt}, true, nil
}
