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

// TrendRow is a single time-bucketed aggregate value.
type TrendRow struct {
	Time  time.Time
	Value float64
}

// BreakdownTrendRow is a single time-bucketed aggregate value with breakdown dimensions.
type BreakdownTrendRow struct {
	Time       time.Time
	Breakdowns []string
	Value      float64
}

// Executor runs pre-built ClickHouse queries and scans the results.
type Executor struct {
	ch driver.Conn
}

// NewExecutor creates an Executor backed by the given ClickHouse connection.
func NewExecutor(ch driver.Conn) *Executor {
	return &Executor{ch: ch}
}

// QueryTrends executes a trends query and returns rows of (time, value).
func (e *Executor) QueryTrends(ctx context.Context, sql string, args []any) ([]TrendRow, error) {
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
		var row TrendRow
		if err := rows.Scan(&row.Time, &row.Value); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// QueryTrendsWithBreakdowns executes a trends query with N breakdown columns and returns rows of
// (time, breakdown_0, ..., breakdown_N-1, value).
func (e *Executor) QueryTrendsWithBreakdowns(ctx context.Context, sql string, args []any, numBreakdowns int) ([]BreakdownTrendRow, error) {
	rows, err := e.ch.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "error closing clickhouse rows", slogx.Error(err))
		}
	}()

	var result []BreakdownTrendRow
	for rows.Next() {
		row := BreakdownTrendRow{
			Breakdowns: make([]string, numBreakdowns),
		}
		// Build scan targets: time, breakdown_0..N-1, value
		dest := make([]any, 0, 2+numBreakdowns)
		dest = append(dest, &row.Time)
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

// EventNameMeta holds count and recency metadata for a single event kind.
type EventNameMeta struct {
	Kind     string
	Count    uint64
	LastSeen time.Time
}

// QueryEventNameMetas executes an event names query against event_names_mv and returns rows of
// (kind, count, last_seen).
func (e *Executor) QueryEventNameMetas(ctx context.Context, sql string, args []any) ([]EventNameMeta, error) {
	rows, err := e.ch.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "error closing clickhouse rows", slogx.Error(err))
		}
	}()

	var result []EventNameMeta
	for rows.Next() {
		var row EventNameMeta
		if err := rows.Scan(&row.Kind, &row.Count, &row.LastSeen); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// QueryDistinctIDs executes a query and returns a list of string values (distinct user IDs).
func (e *Executor) QueryDistinctIDs(ctx context.Context, sql string, args []any) ([]string, error) {
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

// MultiEventTrendRow is a single time-bucketed aggregate value tagged with the event kind.
type MultiEventTrendRow struct {
	Time      time.Time
	EventKind string
	Value     float64
}

// QueryMultiEventTrends executes a UNION ALL trends query and returns rows of (time, event_kind, value).
func (e *Executor) QueryMultiEventTrends(ctx context.Context, sql string, args []any) ([]MultiEventTrendRow, error) {
	rows, err := e.ch.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "error closing clickhouse rows", slogx.Error(err))
		}
	}()

	var result []MultiEventTrendRow
	for rows.Next() {
		var row MultiEventTrendRow
		if err := rows.Scan(&row.Time, &row.EventKind, &row.Value); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// GroupMultiEventSeries groups MultiEventTrendRow results into one Series per event kind.
func GroupMultiEventSeries(rows []MultiEventTrendRow) []*insightsv1.Series {
	var orderedKinds []string
	pointsByKind := map[string][]*insightsv1.DataPoint{}
	for _, r := range rows {
		if _, ok := pointsByKind[r.EventKind]; !ok {
			orderedKinds = append(orderedKinds, r.EventKind)
		}
		pointsByKind[r.EventKind] = append(pointsByKind[r.EventKind], &insightsv1.DataPoint{
			Time:  timestamppb.New(r.Time),
			Value: r.Value,
		})
	}
	series := make([]*insightsv1.Series, 0, len(orderedKinds))
	for _, k := range orderedKinds {
		series = append(series, &insightsv1.Series{
			EventKind: k,
			Points:    pointsByKind[k],
		})
	}
	return series
}

// GroupBreakdownSeries groups BreakdownTrendRow results into Series, keyed by
// their breakdown values. The properties slice provides the property name for
// each breakdown dimension. Insertion order is preserved.
func GroupBreakdownSeries(rows []BreakdownTrendRow, properties []string) []*insightsv1.Series {
	type seriesEntry struct {
		breakdown map[string]string
		points    []*insightsv1.DataPoint
	}
	var orderedKeys []string
	entriesByKey := map[string]*seriesEntry{}

	for _, r := range rows {
		key := fmt.Sprintf("%q", r.Breakdowns)
		if _, ok := entriesByKey[key]; !ok {
			orderedKeys = append(orderedKeys, key)
			bd := make(map[string]string, len(properties))
			for i, prop := range properties {
				bd[prop] = r.Breakdowns[i]
			}
			entriesByKey[key] = &seriesEntry{breakdown: bd}
		}
		entriesByKey[key].points = append(entriesByKey[key].points, &insightsv1.DataPoint{
			Time:  timestamppb.New(r.Time),
			Value: r.Value,
		})
	}

	series := make([]*insightsv1.Series, 0, len(orderedKeys))
	for _, k := range orderedKeys {
		e := entriesByKey[k]
		series = append(series, &insightsv1.Series{
			Breakdown: e.breakdown,
			Points:    e.points,
		})
	}
	return series
}
