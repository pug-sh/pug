package insights

import (
	"context"
	"log/slog"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/fivebitsio/cotton/internal/slogx"
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
