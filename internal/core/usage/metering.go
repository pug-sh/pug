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

	rows, err := s.ch.Query(ctx, meterQuery, from.UTC(), to.UTC())
	if err != nil {
		slog.ErrorContext(ctx, "failed to meter events", slogx.Error(err),
			slog.Time("from", from), slog.Time("to", to))
		telemetry.RecordError(ctx, err)
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
		// toDate arrives stamped in the ClickHouse server's timezone, so .UTC() would
		// shift a non-UTC server's midnight back into the previous day. Take the
		// calendar day as reported and re-stamp it.
		utcDay := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
		out = append(out, DailyUsage{ProjectID: projectID, Day: utcDay, EventCount: int64(count)})
	}
	if err := rows.Err(); err != nil {
		slog.ErrorContext(ctx, "failed while iterating metered usage", slogx.Error(err))
		telemetry.RecordError(ctx, err)
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
	var failed int
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

		s.write.UpsertUsageDaily(ctx, params).Exec(func(i int, err error) {
			if err == nil {
				return
			}
			failed++
			if firstErr == nil {
				firstErr = err
				slog.ErrorContext(ctx, "failed to upsert daily usage", slogx.Error(err),
					slog.String("project_id", chunk[i].ProjectID))
			}
		})
	}
	if firstErr != nil {
		err := fmt.Errorf("usage upsert failed for %d of %d cells: %w", failed, len(usage), firstErr)
		telemetry.RecordError(ctx, err)
		return err
	}
	return nil
}

// DeleteUnmeteredDays drops cells in [from, to) that the pass did not return.
// Their events are gone (GDPR erasure, a dropped partition), and an upsert-only
// pass would leave the stale count standing forever.
func (s *Service) DeleteUnmeteredDays(ctx context.Context, usage []DailyUsage, from, to time.Time) (int64, error) {
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
