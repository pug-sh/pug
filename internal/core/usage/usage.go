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
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	chdb "github.com/pug-sh/pug/internal/deps/clickhouse"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
)

// ErrOrgNotFound reports an org id no org row matches. Distinct from a database
// failure because the caller answers it with NotFound, not Internal.
var ErrOrgNotFound = errors.New("usage: org not found")

// Reader answers the dashboard's two usage questions and nothing else.
//
// The RPC handler holds one of these rather than a Service, so the endpoint
// serving a viewer-floor read has no reachable path to PruneUsage or
// DeleteUnmeteredDays. The meter owns every write in this package; giving the
// read side a type that cannot perform one is cheaper than trusting it not to.
// It also takes no write pool, because it never had a use for one.
type Reader struct {
	read *dbread.Queries
}

func NewReader(pgRO *pgxpool.Pool) *Reader {
	return &Reader{read: dbread.New(pgRO)}
}

// Service is the meter's view: everything a Reader can do, plus the writes and
// the reconcile. Only `pug cron usage` builds one.
type Service struct {
	*Reader

	write *dbwrite.Queries

	// Nil until WithClickHouse; only MeterWindow needs it.
	ch *chdb.Conn
}

// NewService builds the service without a metering source. The write methods are
// live on the result; only MeterWindow is gated. Metering additionally requires
// WithClickHouse.
func NewService(pgRO, pgW *pgxpool.Pool) *Service {
	return &Service{Reader: NewReader(pgRO), write: dbwrite.New(pgW)}
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
func (s *Reader) GetPeriodUsage(ctx context.Context, orgID string, start time.Time) (PeriodUsage, error) {
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
func (s *Reader) periodNotReached(ctx context.Context, orgID string) (PeriodUsage, error) {
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
func (s *Reader) ListDailyUsage(ctx context.Context, orgID string, from, to time.Time) ([]DailyUsage, error) {
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

// GetOrgPeriod resolves the org's current quota window from its anchor. The RPC
// read path and the meter both go through this, so the window a dashboard shows
// and the window the meter sums cannot be derived differently.
func (s *Reader) GetOrgPeriod(ctx context.Context, orgID string, now time.Time) (start, end time.Time, err error) {
	row, err := s.read.GetOrgUsageWindow(ctx, orgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, time.Time{}, ErrOrgNotFound
		}
		slog.ErrorContext(ctx, "failed to read the org usage window", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return time.Time{}, time.Time{}, err
	}
	start, end = PeriodFor(now, AnchorDay(row.CreateTime.Time, anchorOverride(row.AnchorDay)))
	return start, end, nil
}

// AnchorDay is the day of month an org's quota window starts on: the stored
// override when there is one (0 means none), otherwise the day the org was
// created. Every org therefore has a well-defined anchor without billing having
// written anything — see docs/architecture/billing.md section 6.1.
func AnchorDay(orgCreateTime time.Time, override int) int {
	if override >= 1 && override <= 31 {
		return override
	}
	return orgCreateTime.UTC().Day()
}

// anchorOverride unpacks the nullable column. Out of range is impossible under
// the table's check constraint and resolves to "none" anyway, since a period has
// to exist for every org whatever the row says.
func anchorOverride(v pgtype.Int2) int {
	if !v.Valid {
		return 0
	}
	return int(v.Int16)
}

// PeriodFor returns the half-open quota window containing now for an org
// anchored on anchorDay, in UTC. Both bounds land on midnight, which is what
// RefreshPeriodUsage's day-grain sum requires.
func PeriodFor(now time.Time, anchorDay int) (start, end time.Time) {
	u := now.UTC()
	start = anchored(u.Year(), u.Month(), anchorDay)
	if u.Before(start) {
		y, m := shiftMonth(u.Year(), u.Month(), -1)
		start = anchored(y, m, anchorDay)
	}
	y, m := shiftMonth(start.Year(), start.Month(), 1)
	return start, anchored(y, m, anchorDay)
}

// anchored is the UTC midnight of anchorDay within a month, clamped to that
// month's last day. The clamp is the whole point: time.Date NORMALIZES an
// out-of-range day (Feb 31 becomes Mar 3), which would put a period boundary in
// the wrong month rather than at the end of the intended one. Each start is
// re-derived from the anchor, never from the previous start, so a clamped month
// does not drag the anchor down with it: 31 Jan, 28 Feb, 31 Mar.
func anchored(year int, month time.Month, anchorDay int) time.Time {
	if anchorDay < 1 {
		anchorDay = 1
	}
	if last := daysIn(year, month); anchorDay > last {
		anchorDay = last
	}
	return time.Date(year, month, anchorDay, 0, 0, 0, 0, time.UTC)
}

// daysIn is the length of a month: day 0 of the next one normalizes back to it.
func daysIn(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// shiftMonth moves (year, month) by delta months. Anchored to day 1, which every
// month has, so the addition cannot normalize into a neighbouring month the way
// it would from day 31.
func shiftMonth(year int, month time.Month, delta int) (int, time.Month) {
	t := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC).AddDate(0, delta, 0)
	return t.Year(), t.Month()
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

// FloorMonthUTC truncates to the UTC midnight starting t's calendar month.
func FloorMonthUTC(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// CeilDayUTC rounds up to the next UTC midnight, leaving an exact one alone.
func CeilDayUTC(t time.Time) time.Time {
	day := FloorDayUTC(t)
	if day.Equal(t.UTC()) {
		return day
	}
	return day.AddDate(0, 0, 1)
}
