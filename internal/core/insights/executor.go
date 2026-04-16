package insights

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	insightsv1 "github.com/fivebitsio/cotton/internal/gen/proto/shared/insights/v1"
	"github.com/fivebitsio/cotton/internal/slogx"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TrendRow is a single time-bucketed aggregate value with event kind and optional breakdowns.
// All trends queries (single-event, multi-event, with/without breakdowns) produce this row type.
type TrendRow struct {
	Time       time.Time
	EventKind  string
	Breakdowns []string
	Value      float64
}

// FunnelRow is a single funnel step aggregate for one breakdown combination.
type FunnelRow struct {
	StepIndex         int64
	EventKind         string
	Breakdowns        []string
	Value             float64
	AvgConvertSeconds float64 // average seconds from previous step; 0 for step 0 or when timing is not requested
}

// RetentionRow is a single retention aggregate for one cohort bucket, time bucket, and breakdown combination.
type RetentionRow struct {
	CohortTime time.Time
	Time       time.Time
	Value      float64
	CohortSize float64
	Breakdowns []string
}

// Executor runs pre-built ClickHouse queries and scans the results.
type Executor struct {
	ch driver.Conn
}

// NewExecutor creates an Executor backed by the given ClickHouse connection.
func NewExecutor(ch driver.Conn) *Executor {
	if ch == nil {
		panic("insights: ch is nil")
	}
	return &Executor{ch: ch}
}

// QueryTrends executes a trends query and returns rows of (time, event_kind, [breakdown_0..N], value).
func (e *Executor) QueryTrends(ctx context.Context, q TrendsQuery) ([]TrendRow, error) {
	sql := q.SQL()
	args := q.Args()
	rows, err := e.ch.Query(ctx, sql, args...)
	if err != nil {
		slog.ErrorContext(ctx, "clickhouse: query trends failed", slogx.Error(err),
			slog.String("sql", sql), slog.Any("args", args))
		return nil, fmt.Errorf("QueryTrends: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "error closing clickhouse rows", slogx.Error(err))
		}
	}()

	var result []TrendRow
	for rows.Next() {
		row := TrendRow{Breakdowns: make([]string, q.NumBreakdowns())}
		dest := make([]any, 0, 3+q.NumBreakdowns())
		dest = append(dest, &row.Time, &row.EventKind)
		for i := range row.Breakdowns {
			dest = append(dest, &row.Breakdowns[i])
		}
		dest = append(dest, &row.Value)
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("QueryTrends: scan: %w", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("QueryTrends: %w", err)
	}
	return result, nil
}

// QueryScalar executes a query that returns a single float64 value.
func (e *Executor) QueryScalar(ctx context.Context, q ScalarQuery) (float64, error) {
	sql := q.SQL()
	args := q.Args()
	rows, err := e.ch.Query(ctx, sql, args...)
	if err != nil {
		slog.ErrorContext(ctx, "clickhouse: query scalar failed", slogx.Error(err),
			slog.String("sql", sql), slog.Any("args", args))
		return 0, fmt.Errorf("QueryScalar: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "error closing clickhouse rows", slogx.Error(err))
		}
	}()

	var value float64
	if rows.Next() {
		if err := rows.Scan(&value); err != nil {
			return 0, fmt.Errorf("QueryScalar: scan: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("QueryScalar: %w", err)
	}
	return value, nil
}

// AggregateKeyMeta holds count and recency metadata for a key (event kind or property key).
type AggregateKeyMeta struct {
	Key      string
	Count    uint64
	LastSeen time.Time
}

// QueryAggregateKeys executes a query against event_names or property_keys and returns rows of
// (kind/key, count, last_seen).
func (e *Executor) QueryAggregateKeys(ctx context.Context, sql string, args []any) ([]AggregateKeyMeta, error) {
	rows, err := e.ch.Query(ctx, sql, args...)
	if err != nil {
		slog.ErrorContext(ctx, "clickhouse: query aggregate keys failed", slogx.Error(err),
			slog.String("sql", sql), slog.Any("args", args))
		return nil, fmt.Errorf("QueryAggregateKeys: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "error closing clickhouse rows", slogx.Error(err))
		}
	}()

	var result []AggregateKeyMeta
	for rows.Next() {
		var row AggregateKeyMeta
		if err := rows.Scan(&row.Key, &row.Count, &row.LastSeen); err != nil {
			return nil, fmt.Errorf("QueryAggregateKeys: scan: %w", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("QueryAggregateKeys: %w", err)
	}
	return result, nil
}

// QueryStringColumn executes a query and returns a list of string values from the first column.
func (e *Executor) QueryStringColumn(ctx context.Context, sql string, args []any) ([]string, error) {
	rows, err := e.ch.Query(ctx, sql, args...)
	if err != nil {
		slog.ErrorContext(ctx, "clickhouse: query string column failed", slogx.Error(err),
			slog.String("sql", sql), slog.Any("args", args))
		return nil, fmt.Errorf("QueryStringColumn: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "error closing clickhouse rows", slogx.Error(err))
		}
	}()

	var result []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("QueryStringColumn: scan: %w", err)
		}
		result = append(result, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("QueryStringColumn: %w", err)
	}
	return result, nil
}

// QueryFunnel executes a funnel query and returns rows of
// (step_index, event_kind[, breakdown_0..N], value, avg_time_seconds).
func (e *Executor) QueryFunnel(ctx context.Context, q FunnelQuery) ([]FunnelRow, error) {
	sql := q.SQL()
	args := q.Args()
	rows, err := e.ch.Query(ctx, sql, args...)
	if err != nil {
		slog.ErrorContext(ctx, "clickhouse: query funnel failed", slogx.Error(err),
			slog.String("sql", sql), slog.Any("args", args))
		return nil, fmt.Errorf("QueryFunnel: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "error closing clickhouse rows", slogx.Error(err))
		}
	}()

	var result []FunnelRow
	for rows.Next() {
		row := FunnelRow{Breakdowns: make([]string, q.NumBreakdowns())}
		dest := make([]any, 0, 4+q.NumBreakdowns())
		dest = append(dest, &row.StepIndex, &row.EventKind)
		for i := range row.Breakdowns {
			dest = append(dest, &row.Breakdowns[i])
		}
		dest = append(dest, &row.Value, &row.AvgConvertSeconds)
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("QueryFunnel: scan: %w", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("QueryFunnel: %w", err)
	}
	// ClickHouse UNION ALL does not reliably apply a trailing ORDER BY across
	// all branches in every version. Sort client-side to guarantee step order
	// and stable breakdown series ordering.
	slices.SortFunc(result, func(a, b FunnelRow) int {
		for i := range a.Breakdowns {
			if i >= len(b.Breakdowns) {
				return 1
			}
			if c := cmp.Compare(a.Breakdowns[i], b.Breakdowns[i]); c != 0 {
				return c
			}
		}
		if len(a.Breakdowns) < len(b.Breakdowns) {
			return -1
		}
		return cmp.Compare(a.StepIndex, b.StepIndex)
	})
	return result, nil
}

// QueryFunnelUserEvents executes the array-based funnel query and returns per-user
// event arrays for Go-side step matching and timing computation.
// Columns: (distinct_id, times, step_matches[, breakdown_0..N]).
func (e *Executor) QueryFunnelUserEvents(ctx context.Context, q FunnelTimingQuery) ([]FunnelUserEvents, error) {
	sql := q.SQL()
	args := q.Args()
	rows, err := e.ch.Query(ctx, sql, args...)
	if err != nil {
		slog.ErrorContext(ctx, "clickhouse: query funnel user events failed", slogx.Error(err),
			slog.String("sql", sql), slog.Any("args", args))
		return nil, fmt.Errorf("QueryFunnelUserEvents: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "error closing clickhouse rows", slogx.Error(err))
		}
	}()

	var result []FunnelUserEvents
	for rows.Next() {
		row := FunnelUserEvents{Breakdowns: make([]string, q.NumBreakdowns())}
		dest := make([]any, 0, 3+q.NumBreakdowns())
		dest = append(dest, &row.DistinctID, &row.Times, &row.StepMatches)
		for i := range row.Breakdowns {
			dest = append(dest, &row.Breakdowns[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("QueryFunnelUserEvents: scan: %w", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("QueryFunnelUserEvents: %w", err)
	}
	return result, nil
}

// QueryRetention executes a retention query and returns rows of
// (cohort_time, time, value, cohort_size[, breakdown_0..N]).
func (e *Executor) QueryRetention(ctx context.Context, q RetentionQuery) ([]RetentionRow, error) {
	sql := q.SQL()
	args := q.Args()
	rows, err := e.ch.Query(ctx, sql, args...)
	if err != nil {
		slog.ErrorContext(ctx, "clickhouse: query retention failed", slogx.Error(err),
			slog.String("sql", sql), slog.Any("args", args))
		return nil, fmt.Errorf("QueryRetention: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "error closing clickhouse rows", slogx.Error(err))
		}
	}()

	var result []RetentionRow
	for rows.Next() {
		row := RetentionRow{Breakdowns: make([]string, q.NumBreakdowns())}
		dest := make([]any, 0, 4+q.NumBreakdowns())
		dest = append(dest, &row.CohortTime, &row.Time, &row.Value, &row.CohortSize)
		for i := range row.Breakdowns {
			dest = append(dest, &row.Breakdowns[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("QueryRetention: scan: %w", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("QueryRetention: %w", err)
	}
	return result, nil
}

// breakdownKey joins breakdown values into a single map key using a null-byte separator.
// Breakdown values come from ClickHouse string columns and cannot contain null bytes.
func breakdownKey(vals []string) string {
	return strings.Join(vals, "\x00")
}

// GroupSeries groups TrendRow results into TrendSeries, keyed by (event_kind, breakdown_tuple).
// The properties slice provides the property name for each breakdown dimension.
// Insertion order is preserved.
func GroupSeries(rows []TrendRow, properties []string) ([]*insightsv1.TrendSeries, error) {
	type seriesKey struct {
		eventKind string
		breakdown string
	}
	type seriesEntry struct {
		eventKind string
		breakdown map[string]string
		points    []*insightsv1.DataPoint
	}
	var orderedKeys []seriesKey
	entriesByKey := map[seriesKey]*seriesEntry{}

	for _, r := range rows {
		if len(r.Breakdowns) != len(properties) {
			return nil, fmt.Errorf("row has %d breakdowns but expected %d", len(r.Breakdowns), len(properties))
		}
		key := seriesKey{eventKind: r.EventKind, breakdown: breakdownKey(r.Breakdowns)}
		if _, ok := entriesByKey[key]; !ok {
			orderedKeys = append(orderedKeys, key)
			bd := make(map[string]string, len(properties))
			for i, prop := range properties {
				bd[prop] = r.Breakdowns[i]
			}
			entriesByKey[key] = &seriesEntry{eventKind: r.EventKind, breakdown: bd}
		}
		entriesByKey[key].points = append(entriesByKey[key].points, &insightsv1.DataPoint{
			Time:  timestamppb.New(r.Time),
			Value: proto.Float64(r.Value),
		})
	}

	series := make([]*insightsv1.TrendSeries, 0, len(orderedKeys))
	for _, k := range orderedKeys {
		e := entriesByKey[k]
		// ClickHouse UNION ALL does not reliably apply a trailing ORDER BY across
		// all branches in every version. Sort client-side to guarantee time order.
		slices.SortStableFunc(e.points, func(a, b *insightsv1.DataPoint) int {
			return a.GetTime().AsTime().Compare(b.GetTime().AsTime())
		})
		s := &insightsv1.TrendSeries{
			EventKind: proto.String(e.eventKind),
			Points:    e.points,
		}
		if len(e.breakdown) > 0 {
			s.Breakdown = e.breakdown
		}
		series = append(series, s)
	}
	return series, nil
}

// GroupFunnelSeries groups FunnelRow results into FunnelSeries, keyed by breakdown tuple.
// The properties slice provides the property name for each breakdown dimension.
func GroupFunnelSeries(rows []FunnelRow, properties []string) ([]*insightsv1.FunnelSeries, error) {
	type seriesEntry struct {
		breakdown map[string]string
		steps     []*insightsv1.FunnelStep
	}

	var orderedKeys []string
	entriesByKey := map[string]*seriesEntry{}

	for _, r := range rows {
		if len(r.Breakdowns) != len(properties) {
			return nil, fmt.Errorf("funnel row has %d breakdowns but expected %d", len(r.Breakdowns), len(properties))
		}
		key := breakdownKey(r.Breakdowns)
		if _, ok := entriesByKey[key]; !ok {
			orderedKeys = append(orderedKeys, key)
			bd := make(map[string]string, len(properties))
			for i, prop := range properties {
				bd[prop] = r.Breakdowns[i]
			}
			entriesByKey[key] = &seriesEntry{breakdown: bd}
		}
		entriesByKey[key].steps = append(entriesByKey[key].steps, &insightsv1.FunnelStep{
			EventKind:               proto.String(r.EventKind),
			Total:                   proto.Float64(r.Value),
			AvgTimeToConvertSeconds: proto.Float64(r.AvgConvertSeconds),
		})
	}

	series := make([]*insightsv1.FunnelSeries, 0, len(orderedKeys))
	for _, k := range orderedKeys {
		e := entriesByKey[k]
		s := &insightsv1.FunnelSeries{Steps: e.steps}
		if len(e.breakdown) > 0 {
			s.Breakdown = e.breakdown
		}
		series = append(series, s)
	}
	return series, nil
}

// GroupRetentionSeries groups RetentionRow results into RetentionSeries, keyed by breakdown tuple.
// Within each series, rows are grouped into cohorts. Insertion order is preserved for both
// series and cohorts.
func GroupRetentionSeries(rows []RetentionRow, properties []string) ([]*insightsv1.RetentionSeries, error) {
	type cohortEntry struct {
		order  []time.Time
		byTime map[time.Time]*insightsv1.RetentionCohort
	}
	type seriesEntry struct {
		series  *insightsv1.RetentionSeries
		cohorts cohortEntry
	}

	var orderedKeys []string
	entriesByKey := map[string]*seriesEntry{}

	for _, row := range rows {
		if len(row.Breakdowns) != len(properties) {
			return nil, fmt.Errorf("retention row has %d breakdowns but expected %d", len(row.Breakdowns), len(properties))
		}
		key := breakdownKey(row.Breakdowns)
		if _, ok := entriesByKey[key]; !ok {
			orderedKeys = append(orderedKeys, key)
			bd := make(map[string]string, len(properties))
			for i, prop := range properties {
				bd[prop] = row.Breakdowns[i]
			}
			rs := &insightsv1.RetentionSeries{}
			if len(bd) > 0 {
				rs.Breakdown = bd
			}
			entriesByKey[key] = &seriesEntry{
				series:  rs,
				cohorts: cohortEntry{byTime: map[time.Time]*insightsv1.RetentionCohort{}},
			}
		}
		entry := entriesByKey[key]
		if _, ok := entry.cohorts.byTime[row.CohortTime]; !ok {
			entry.cohorts.order = append(entry.cohorts.order, row.CohortTime)
			entry.cohorts.byTime[row.CohortTime] = &insightsv1.RetentionCohort{
				Cohort:     proto.String(row.CohortTime.Format(time.RFC3339)),
				CohortSize: proto.Float64(row.CohortSize),
			}
		}
		entry.cohorts.byTime[row.CohortTime].Points = append(entry.cohorts.byTime[row.CohortTime].Points, &insightsv1.DataPoint{
			Time:  timestamppb.New(row.Time),
			Value: proto.Float64(row.Value),
		})
	}

	out := make([]*insightsv1.RetentionSeries, 0, len(orderedKeys))
	for _, key := range orderedKeys {
		entry := entriesByKey[key]
		for _, ct := range entry.cohorts.order {
			entry.series.Cohorts = append(entry.series.Cohorts, entry.cohorts.byTime[ct])
		}
		out = append(out, entry.series)
	}
	return out, nil
}
