package usage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
)

// uniqExact over raw events, no FINAL — see docs/architecture/usage.md §2.
//
// toDate's 'UTC' is explicit because occur_time is DateTime64(3) with no declared
// zone, so a bare toDate() would cut days on the ClickHouse server's midnight.
const meterQuery = `
select project_id, toDate(occur_time, 'UTC') as day, uniqExact(event_id) as event_count
from events
where occur_time >= ? and occur_time < ?
group by project_id, day
`

// ErrNoMeteringConn is returned when a Service built without ClickHouse meters —
// the server never does.
var ErrNoMeteringConn = errors.New("usage: no clickhouse connection for metering")

// DailyUsage is one metered (project, day) cell.
type DailyUsage struct {
	ProjectID  string
	Day        time.Time
	EventCount int64
}

// MeterWindow counts distinct events per project per day over [from, to), in no
// particular row order. The (project, day) grain lets one pass serve every org's
// period, and it bounds uniqExact's state.
func (s *Service) MeterWindow(ctx context.Context, from, to time.Time) ([]DailyUsage, error) {
	if s.ch == nil {
		return nil, ErrNoMeteringConn
	}

	// chdb.Conn.Query records the error on the ClickHouse span already, so this
	// logs the window it failed over without re-recording it.
	rows, err := s.ch.Query(ctx, meterQuery, from.UTC(), to.UTC())
	if err != nil {
		slog.ErrorContext(ctx, "failed to meter events", slogx.Error(err),
			slog.Time("from", from), slog.Time("to", to))
		return nil, err
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			slog.WarnContext(ctx, "failed to close metering rows", slogx.Error(closeErr))
		}
	}()

	var out []DailyUsage
	for rows.Next() {
		var (
			projectID string
			day       time.Time
			count     uint64
		)
		if err := rows.Scan(&projectID, &day, &count); err != nil {
			slog.ErrorContext(ctx, "failed to scan metered usage", slogx.Error(err))
			telemetry.RecordError(ctx, err)
			return nil, err
		}
		// toDate arrives stamped in the ClickHouse server's timezone. Converting that
		// to UTC -- which postgres.NewDate does on the way to storage -- would shift
		// midnight on a server *east* of UTC back into the previous day (Asia/Kolkata
		// midnight is 18:30Z the day before). Take the calendar day as reported and
		// re-stamp it, so what gets stored is the day ClickHouse named.
		utcDay := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
		out = append(out, DailyUsage{ProjectID: projectID, Day: utcDay, EventCount: int64(count)})
	}
	// Recorded on the ClickHouse span by tracedRows.Err; log only.
	if err := rows.Err(); err != nil {
		slog.ErrorContext(ctx, "failed while iterating metered usage", slogx.Error(err))
		return nil, err
	}
	return out, nil
}

// Bounds the pgx batch, not the buffer — MeterWindow has already materialized
// every cell by the time this runs.
const usageBatchChunk = 1000

// RecordDailyUsage upserts metered cells in pipelined batches. A cell whose
// project is gone from Postgres writes nothing; see UpsertUsageDaily.
func (s *Service) RecordDailyUsage(ctx context.Context, usage []DailyUsage) error {
	var firstErr error
	failedChunkStart := -1
	for start := 0; start < len(usage) && firstErr == nil; start += usageBatchChunk {
		chunk := usage[start:min(start+usageBatchChunk, len(usage))]

		params := make([]dbwrite.UpsertUsageDailyParams, 0, len(chunk))
		for _, u := range chunk {
			params = append(params, dbwrite.UpsertUsageDailyParams{
				Day:        postgres.NewDate(u.Day),
				EventCount: u.EventCount,
				ProjectID:  u.ProjectID,
			})
		}

		// pgx latches the first error and hands it back from every later Exec without
		// touching the wire, so counting callback invocations would count results
		// still pending, not cells that failed. Record where the batch died instead.
		// Postgres runs a chunk under one implicit transaction (pgx sends a single
		// trailing Sync), so the whole chunk rolled back either way.
		br := s.write.UpsertUsageDaily(ctx, params)
		br.Exec(func(i int, err error) {
			if err == nil || firstErr != nil {
				return
			}
			firstErr = err
			failedChunkStart = start
			slog.ErrorContext(ctx, "failed to upsert daily usage", slogx.Error(err),
				slog.String("project_id", chunk[i].ProjectID),
				slog.Time("day", chunk[i].Day))
		})
		// Exec's generated body closes the batch but throws Close's error away, and
		// the trailing Sync/ReadyForQuery is read there rather than by any Exec. A
		// chunk that dies after its last statement result -- connection loss, a
		// failure surfacing at the implicit transaction's commit -- therefore reaches
		// no callback. Closing again returns the error pgx latched (a second Close on
		// a clean batch is a nil-returning no-op), so the pass cannot go on to stamp
		// usage_computed_at over counts it never wrote.
		if closeErr := br.Close(); closeErr != nil && firstErr == nil {
			firstErr = closeErr
			failedChunkStart = start
			slog.ErrorContext(ctx, "daily usage batch failed at close", slogx.Error(closeErr),
				slog.Int("chunk_start", start), slog.Int("chunk_size", len(chunk)))
		}
	}
	if firstErr != nil {
		// Everything from the failing chunk onward is unwritten: that chunk rolled
		// back and the loop never issued the ones after it.
		err := fmt.Errorf("usage upsert failed in the chunk starting at cell %d; %d of %d cells unwritten: %w",
			failedChunkStart, len(usage)-failedChunkStart, len(usage), firstErr)
		telemetry.RecordError(ctx, err)
		return err
	}
	return nil
}

// CountStoredDays reports how many day cells are stored over [from, to), across
// every org. The meter uses it to tell a genuinely idle window apart from a
// ClickHouse read that came back empty when it should not have.
func (s *Service) CountStoredDays(ctx context.Context, from, to time.Time) (int64, error) {
	n, err := s.read.CountUsageDailyInRange(ctx, dbread.CountUsageDailyInRangeParams{
		FromDay: postgres.NewDate(FloorDayUTC(from)),
		ToDay:   postgres.NewDate(CeilDayUTC(to)),
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to count stored usage days", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return 0, err
	}
	return n, nil
}

// CountKnownProjects reports how many of the given project ids exist in Postgres.
// The meter uses it to tell a routine deleted project apart from a Postgres that
// does not know the projects ClickHouse is reporting at all — the second would
// otherwise reconcile every stored cell away and stamp every org with a zero,
// because an unknown project's upsert writes nothing and reports success.
func (s *Service) CountKnownProjects(ctx context.Context, projectIDs []string) (int64, error) {
	if len(projectIDs) == 0 {
		return 0, nil
	}
	n, err := s.read.CountKnownProjectIDs(ctx, projectIDs)
	if err != nil {
		slog.ErrorContext(ctx, "failed to count known projects", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return 0, err
	}
	return n, nil
}

// ProjectIDs returns the distinct project ids in a metered slice, in first-seen
// order.
func ProjectIDs(usage []DailyUsage) []string {
	seen := make(map[string]struct{}, len(usage))
	out := make([]string, 0, len(usage))
	for _, u := range usage {
		if _, ok := seen[u.ProjectID]; ok {
			continue
		}
		seen[u.ProjectID] = struct{}{}
		out = append(out, u.ProjectID)
	}
	return out
}

// DeleteUnmeteredDays drops cells in [from, to) that the pass did not return.
// Their events are gone (GDPR erasure, a dropped partition), and an upsert-only
// pass would leave the stale count standing forever.
//
// An empty usage deletes nothing rather than everything: with no cells to keep,
// every stored row in the window is "unmetered" and the statement would wipe it.
// An empty read is a bad read, never a reconcile.
func (s *Service) DeleteUnmeteredDays(ctx context.Context, usage []DailyUsage, from, to time.Time) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	// Fails closed here, not just at the caller: this is the most destructive
	// statement in the feature and it is exported.
	if len(usage) == 0 {
		return 0, nil
	}

	projectIDs := make([]string, 0, len(usage))
	days := make([]pgtype.Date, 0, len(usage))
	for _, u := range usage {
		projectIDs = append(projectIDs, u.ProjectID)
		days = append(days, postgres.NewDate(u.Day))
	}

	n, err := s.write.DeleteUnmeteredUsageDaily(ctx, dbwrite.DeleteUnmeteredUsageDailyParams{
		FromDay:    postgres.NewDate(FloorDayUTC(from)),
		ToDay:      postgres.NewDate(CeilDayUTC(to)),
		ProjectIds: projectIDs,
		Days:       days,
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to drop unmetered usage days", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return 0, err
	}
	return n, nil
}

// RefreshPeriodUsage re-sums and stores an org's period. A day cannot split, so
// it falls in the period containing its own UTC midnight.
func (s *Service) RefreshPeriodUsage(ctx context.Context, orgID string, start, end time.Time) (int64, error) {
	total, err := s.write.RefreshUsagePeriod(ctx, dbwrite.RefreshUsagePeriodParams{
		FromDay:     postgres.NewDate(CeilDayUTC(start)),
		OrgID:       orgID,
		PeriodEnd:   postgres.NewTimestamptz(end),
		PeriodStart: postgres.NewTimestamptz(start),
		ToDay:       postgres.NewDate(CeilDayUTC(end)),
	})
	if err != nil {
		slog.ErrorContext(ctx, "failed to refresh period usage", slogx.Error(err), slog.String("org_id", orgID))
		telemetry.RecordError(ctx, err)
		return 0, err
	}
	return total, nil
}

// PruneUsage drops day rows older than the retention window.
func (s *Service) PruneUsage(ctx context.Context, olderThan time.Time) (int64, error) {
	n, err := s.write.PruneUsageDaily(ctx, postgres.NewDate(CeilDayUTC(olderThan)))
	if err != nil {
		slog.ErrorContext(ctx, "failed to prune usage", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return 0, err
	}
	return n, nil
}

// OrgPeriod is one org's current usage window.
type OrgPeriod struct {
	OrgID      string
	Start, End time.Time
}

// OrgPeriods is the meter's work list: every org, on the UTC calendar month.
// Driven from orgs, so an org that has never sent an event still gets a row and
// reads as a metered zero.
func (s *Service) OrgPeriods(ctx context.Context, now time.Time) ([]OrgPeriod, error) {
	ids, err := s.read.ListOrgIDsForUsage(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list orgs for metering", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return nil, err
	}

	start, end := CalendarMonth(now)
	out := make([]OrgPeriod, 0, len(ids))
	for _, id := range ids {
		out = append(out, OrgPeriod{OrgID: id, Start: start, End: end})
	}
	return out, nil
}
