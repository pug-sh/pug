package insights

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	insightsv1 "github.com/fivebitsio/cotton/internal/gen/proto/shared/insights/v1"
	"github.com/fivebitsio/cotton/internal/slogx"
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

// FunnelRow is a single funnel step aggregate.
type FunnelRow struct {
	StepIndex int64
	EventKind string
	Value     float64
}

// RetentionRow is a single retention aggregate for one cohort bucket and time bucket.
type RetentionRow struct {
	CohortTime time.Time
	Time       time.Time
	Value      float64
	CohortSize float64
}

// Executor runs pre-built ClickHouse queries and scans the results.
type Executor struct {
	ch driver.Conn
}

// NewExecutor creates an Executor backed by the given ClickHouse connection.
func NewExecutor(ch driver.Conn) *Executor {
	return &Executor{ch: ch}
}

// QueryTrends executes a trends query and returns rows of (time, event_kind, [breakdown_0..N], value).
// numBreakdowns indicates how many breakdown columns to scan between event_kind and value.
func (e *Executor) QueryTrends(ctx context.Context, sql string, args []any, numBreakdowns int) ([]TrendRow, error) {
	rows, err := e.ch.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "error closing clickhouse rows", slogx.Error(err))
		}
	}()

	var result []TrendRow
	for rows.Next() {
		row := TrendRow{Breakdowns: make([]string, numBreakdowns)}
		dest := make([]any, 0, 3+numBreakdowns)
		dest = append(dest, &row.Time, &row.EventKind)
		for i := range row.Breakdowns {
			dest = append(dest, &row.Breakdowns[i])
		}
		dest = append(dest, &row.Value)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// QueryScalar executes a query that returns a single float64 value.
func (e *Executor) QueryScalar(ctx context.Context, sql string, args []any) (float64, error) {
	rows, err := e.ch.Query(ctx, sql, args...)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "error closing clickhouse rows", slogx.Error(err))
		}
	}()

	var value float64
	if rows.Next() {
		if err := rows.Scan(&value); err != nil {
			return 0, err
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
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
		return nil, err
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
			return nil, err
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// QueryStringColumn executes a query and returns a list of string values from the first column.
func (e *Executor) QueryStringColumn(ctx context.Context, sql string, args []any) ([]string, error) {
	rows, err := e.ch.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
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
			return nil, err
		}
		result = append(result, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// QueryFunnel executes a funnel query and returns rows of (step_index, event_kind, value).
func (e *Executor) QueryFunnel(ctx context.Context, sql string, args []any) ([]FunnelRow, error) {
	rows, err := e.ch.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "error closing clickhouse rows", slogx.Error(err))
		}
	}()

	var result []FunnelRow
	for rows.Next() {
		var row FunnelRow
		if err := rows.Scan(&row.StepIndex, &row.EventKind, &row.Value); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// QueryRetention executes a retention query and returns rows of (cohort_time, time, value, cohort_size).
func (e *Executor) QueryRetention(ctx context.Context, sql string, args []any) ([]RetentionRow, error) {
	rows, err := e.ch.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "error closing clickhouse rows", slogx.Error(err))
		}
	}()

	var result []RetentionRow
	for rows.Next() {
		var row RetentionRow
		if err := rows.Scan(&row.CohortTime, &row.Time, &row.Value, &row.CohortSize); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// GroupSeries groups TrendRow results into Series, keyed by (event_kind, breakdown_tuple).
// The properties slice provides the property name for each breakdown dimension.
// Insertion order is preserved.
func GroupSeries(rows []TrendRow, properties []string) []*insightsv1.Series {
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
		if len(properties) > 0 && len(r.Breakdowns) < len(properties) {
			return nil
		}
		key := seriesKey{eventKind: r.EventKind, breakdown: fmt.Sprintf("%q", r.Breakdowns)}
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
			Value: r.Value,
		})
	}

	series := make([]*insightsv1.Series, 0, len(orderedKeys))
	for _, k := range orderedKeys {
		e := entriesByKey[k]
		s := &insightsv1.Series{
			EventKind: e.eventKind,
			Points:    e.points,
		}
		if len(e.breakdown) > 0 {
			s.Breakdown = e.breakdown
		}
		series = append(series, s)
	}
	return series
}

// GroupRetentionSeries groups retention rows into one series per cohort.
func GroupRetentionSeries(rows []RetentionRow) []*insightsv1.Series {
	seriesByCohort := map[time.Time]*insightsv1.Series{}
	order := make([]time.Time, 0)

	for _, row := range rows {
		if _, ok := seriesByCohort[row.CohortTime]; !ok {
			order = append(order, row.CohortTime)
			seriesByCohort[row.CohortTime] = &insightsv1.Series{
				Breakdown: map[string]string{
					"cohort": row.CohortTime.Format(time.RFC3339),
				},
				Total: row.CohortSize,
			}
		}
		seriesByCohort[row.CohortTime].Points = append(seriesByCohort[row.CohortTime].Points, &insightsv1.DataPoint{
			Time:  timestamppb.New(row.Time),
			Value: row.Value,
		})
	}

	out := make([]*insightsv1.Series, 0, len(order))
	for _, cohort := range order {
		out = append(out, seriesByCohort[cohort])
	}
	return out
}
