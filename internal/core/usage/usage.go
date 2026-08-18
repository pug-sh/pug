// Package usage counts the events an org has sent, at (project, day) grain.
//
// It is reporting, never enforcement. The meter reads ClickHouse well after
// ingestion has committed, so nothing here can reject, throttle or delay an
// event, and a stalled meter degrades observability rather than the product.
package usage

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	chdb "github.com/pug-sh/pug/internal/deps/clickhouse"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
)

// Service is the whole package: the dashboard's two reads, plus the writes and
// the reconcile the meter owns.
type Service struct {
	read  *dbread.Queries
	write *dbwrite.Queries

	// Nil until WithClickHouse; only MeterWindow needs it.
	ch *chdb.Conn
}

// NewService builds the service without a metering source. The write methods are
// live on the result; only MeterWindow is gated. Metering additionally requires
// WithClickHouse.
func NewService(pgRO, pgW *pgxpool.Pool) *Service {
	return &Service{read: dbread.New(pgRO), write: dbwrite.New(pgW)}
}

// WithClickHouse returns a copy with the metering connection attached. Only
// `pug cron usage` needs it; the server answers every read from Postgres alone.
// Value receiver so this really is the copy-returning builder it reads as.
func (s Service) WithClickHouse(conn *chdb.Conn) *Service {
	s.ch = conn
	return &s
}

// PeriodUsage is the stored per-period total plus how stale it is.
//
// Three states, and EventCount is only a measurement in the first:
//
//   - Counted: the meter has summed this period. EventCount is its total.
//   - !Counted, UsageComputedAt set: the meter has run for this org but has not
//     reached this period yet (a month rollover). EventCount is meaningless.
//   - !Counted, UsageComputedAt zero: the meter has never run. No answer at all.
//
// Counted is what keeps the second state from reading as a metered zero. Without
// it the caller would have to infer the difference by comparing the stamp against
// the period start, which is the client-side arithmetic the wire shape exists to
// avoid.
type PeriodUsage struct {
	EventCount      int64
	UsageComputedAt time.Time
	Counted         bool
}

// GetPeriodUsage reads the pre-summed period total.
func (s *Service) GetPeriodUsage(ctx context.Context, orgID string, start time.Time) (PeriodUsage, error) {
	row, err := s.read.GetUsagePeriod(ctx, dbread.GetUsagePeriodParams{
		OrgID:       orgID,
		PeriodStart: postgres.NewTimestamptz(start),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return s.periodNotReached(ctx, orgID)
		}
		slog.ErrorContext(ctx, "failed to read period usage", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return PeriodUsage{}, err
	}
	return PeriodUsage{
		EventCount:      row.EventCount,
		UsageComputedAt: row.UsageComputedAt.Time,
		Counted:         true,
	}, nil
}

// A month rollover leaves the new period rowless until the next pass. That is a
// metered org with nothing counted yet, not an unmetered one, so it keeps the
// org's last stamp — otherwise every dashboard reads "never metered" on the 1st.
// Counted stays false: the stamp says the meter is alive, not that it has summed
// this period.
func (s *Service) periodNotReached(ctx context.Context, orgID string) (PeriodUsage, error) {
	at, err := s.read.GetLatestUsageComputedAt(ctx, orgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PeriodUsage{}, nil
		}
		slog.ErrorContext(ctx, "failed to read the last usage stamp", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return PeriodUsage{}, err
	}
	return PeriodUsage{UsageComputedAt: at.Time}, nil
}

// MaxDailyRows caps one ListDailyUsage read. A row is (day x project): the
// request's 400-day span cap bounds the days, nothing bounds the projects, so
// without this a large org's 400-day request materializes the product. 10k rows
// covers a year for 27 projects, or 400 days for 25 — past that the series is a
// chart nobody reads, and the caller is better served by a narrower window.
const MaxDailyRows = 10_000

// ListDailyUsage returns the org's stored (project, day) cells over [from, to),
// oldest first, at most MaxDailyRows of them. Bounds snap outwards to whole UTC
// days.
func (s *Service) ListDailyUsage(ctx context.Context, orgID string, from, to time.Time) ([]DailyUsage, error) {
	rows, err := s.read.ListUsageDailyByOrgID(ctx, dbread.ListUsageDailyByOrgIDParams{
		FromDay:  postgres.NewDate(FloorDayUTC(from)),
		OrgID:    orgID,
		ToDay:    postgres.NewDate(CeilDayUTC(to)),
		RowLimit: MaxDailyRows,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to list daily usage", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return nil, err
	}
	out := make([]DailyUsage, 0, len(rows))
	for _, row := range rows {
		out = append(out, DailyUsage{
			ProjectID:  row.ProjectID,
			Day:        row.Day.Time,
			EventCount: row.EventCount,
		})
	}
	return out, nil
}

// CalendarMonth returns the UTC calendar month containing now, half-open.
func CalendarMonth(now time.Time) (start, end time.Time) {
	u := now.UTC()
	start = time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 1, 0)
}

// FloorDayUTC truncates to the UTC midnight starting t's day. Calendar-explicit
// rather than Truncate(24h): both land on the same instant for a UTC-normalized
// value, but Truncate floors relative to the zero time rather than to a calendar
// day, so it would start cutting the wrong boundary if the .UTC() below were ever
// dropped.
func FloorDayUTC(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// CeilDayUTC rounds up to the next UTC midnight, leaving an exact one alone.
func CeilDayUTC(t time.Time) time.Time {
	day := FloorDayUTC(t)
	if day.Equal(t.UTC()) {
		return day
	}
	return day.AddDate(0, 0, 1)
}
