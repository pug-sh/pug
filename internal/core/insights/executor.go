package insights

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/slogx"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
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
// Timing is nil for step 0 (entry step has no conversion time) and when timing is not requested.
type FunnelRow struct {
	StepIndex  int64
	EventKind  string
	Breakdowns []string
	Value      float64
	Timing     *StepTiming
}

// StepTiming holds the conversion-time statistics for one funnel step relative to the previous one.
// Instances produced by ComputeFunnelTiming have Distribution of length len(funnelTimingBuckets);
// callers constructing StepTiming directly (e.g. tests) are responsible for that invariant.
type StepTiming struct {
	Avg          time.Duration
	Median       time.Duration
	P95          time.Duration
	Distribution []int64
}

// UserFlowRow is a single step-scoped source→target edge aggregate from
// QueryUserFlow. Step is the 0-based depth of Source (the Sankey column); the
// edge connects depth Step → Step+1, so Target lives at depth Step+1.
type UserFlowRow struct {
	Step   int32
	Source string
	Target string
	Value  int64
}

// RetentionRow is a single retention aggregate for one cohort bucket, time bucket, and breakdown combination.
type RetentionRow struct {
	CohortTime time.Time
	Time       time.Time
	Value      float64
	CohortSize float64
	Breakdowns []string
}

// maxLoggedSQLLen bounds the SQL length attached to error logs. Trends/funnel/retention
// queries with breakdowns and top_vals can run several KB; on sustained ClickHouse
// timeouts that multiplies log volume. Truncated SQL still identifies the failing query
// shape; the wrapped error and arg_count attribute carry the full failure context.
const maxLoggedSQLLen = 2048

func truncateSQL(sql string) string {
	if len(sql) <= maxLoggedSQLLen {
		return sql
	}
	return sql[:maxLoggedSQLLen] + "...[truncated]"
}

// isContextError reports whether err is a context cancellation or deadline — a
// client/request-lifecycle condition, not a ClickHouse fault. Such errors are
// not logged or recorded (a disconnected or timed-out caller would otherwise
// manufacture error-rate noise); the wrapped error still propagates so the
// caller can map it to the right status.
func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// recordQueryError logs and records a failed query execution at the layer that
// detects it (per telemetry.md), skipping client context cancellation/deadline.
func recordQueryError(ctx context.Context, msg, projectID, sql string, argCount int, err error) {
	if isContextError(err) {
		return
	}
	slog.ErrorContext(ctx, msg, slogx.Error(err),
		slog.String("project_id", projectID), slog.String("sql", truncateSQL(sql)), slog.Int("arg_count", argCount))
	telemetry.RecordError(ctx, err)
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
func (e *Executor) QueryTrends(ctx context.Context, projectID string, q TrendsQuery) ([]TrendRow, error) {
	sql := q.SQL()
	args := q.Args()
	rows, err := e.ch.Query(ctx, sql, args...)
	if err != nil {
		recordQueryError(ctx, "clickhouse: query trends failed", projectID, sql, len(args), err)
		return nil, fmt.Errorf("QueryTrends: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "error closing clickhouse rows", slogx.Error(err),
				slog.String("project_id", projectID))
			telemetry.RecordError(ctx, err)
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
			slog.ErrorContext(ctx, "clickhouse: query trends scan failed", slogx.Error(err),
				slog.String("project_id", projectID))
			telemetry.RecordError(ctx, err)
			return nil, fmt.Errorf("QueryTrends: scan: %w", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		slog.ErrorContext(ctx, "clickhouse: query trends iteration failed", slogx.Error(err),
			slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return nil, fmt.Errorf("QueryTrends: %w", err)
	}
	return result, nil
}

// QueryScalar executes a query that returns a single float64 value.
func (e *Executor) QueryScalar(ctx context.Context, projectID string, q ScalarQuery) (float64, error) {
	sql := q.SQL()
	args := q.Args()
	rows, err := e.ch.Query(ctx, sql, args...)
	if err != nil {
		recordQueryError(ctx, "clickhouse: query scalar failed", projectID, sql, len(args), err)
		return 0, fmt.Errorf("QueryScalar: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "error closing clickhouse rows", slogx.Error(err),
				slog.String("project_id", projectID))
			telemetry.RecordError(ctx, err)
		}
	}()

	var value float64
	if rows.Next() {
		if err := rows.Scan(&value); err != nil {
			slog.ErrorContext(ctx, "clickhouse: query scalar scan failed", slogx.Error(err),
				slog.String("project_id", projectID))
			telemetry.RecordError(ctx, err)
			return 0, fmt.Errorf("QueryScalar: scan: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		slog.ErrorContext(ctx, "clickhouse: query scalar iteration failed", slogx.Error(err),
			slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return 0, fmt.Errorf("QueryScalar: %w", err)
	}
	return value, nil
}

// AggregateKeyMeta holds count and recency metadata for a key (event kind or property key).
// ValueType is populated only when the query selects a value_type column (property_keys queries);
// it is empty for event_names queries which do not have that column.
type AggregateKeyMeta struct {
	Key       string
	ValueType string
	Count     uint64
	LastSeen  time.Time
}

// QueryAggregateKeys executes a query against event_names or property_keys and returns rows of
// (kind/key[, value_type], count, last_seen). The value_type column is optional: property_keys
// queries emit 4 columns; event_names queries emit 3. Column count is detected via rows.Columns().
func (e *Executor) QueryAggregateKeys(ctx context.Context, projectID string, sql string, args []any) ([]AggregateKeyMeta, error) {
	rows, err := e.ch.Query(ctx, sql, args...)
	if err != nil {
		recordQueryError(ctx, "clickhouse: query aggregate keys failed", projectID, sql, len(args), err)
		return nil, fmt.Errorf("QueryAggregateKeys: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "error closing clickhouse rows", slogx.Error(err),
				slog.String("project_id", projectID))
			telemetry.RecordError(ctx, err)
		}
	}()

	// Dispatch on the presence of the value_type column rather than column count —
	// less brittle if either query ever adds an unrelated projection.
	hasValueType := slices.Contains(rows.Columns(), "value_type")

	var result []AggregateKeyMeta
	for rows.Next() {
		var row AggregateKeyMeta
		var scanErr error
		if hasValueType {
			scanErr = rows.Scan(&row.Key, &row.ValueType, &row.Count, &row.LastSeen)
		} else {
			scanErr = rows.Scan(&row.Key, &row.Count, &row.LastSeen)
		}
		if scanErr != nil {
			slog.ErrorContext(ctx, "clickhouse: query aggregate keys scan failed", slogx.Error(scanErr),
				slog.String("project_id", projectID))
			telemetry.RecordError(ctx, scanErr)
			return nil, fmt.Errorf("QueryAggregateKeys: scan: %w", scanErr)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		slog.ErrorContext(ctx, "clickhouse: query aggregate keys iteration failed", slogx.Error(err),
			slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return nil, fmt.Errorf("QueryAggregateKeys: %w", err)
	}
	return result, nil
}

// QueryStringColumn executes a query and returns a list of string values from the first column.
func (e *Executor) QueryStringColumn(ctx context.Context, projectID string, sql string, args []any) ([]string, error) {
	rows, err := e.ch.Query(ctx, sql, args...)
	if err != nil {
		recordQueryError(ctx, "clickhouse: query string column failed", projectID, sql, len(args), err)
		return nil, fmt.Errorf("QueryStringColumn: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "error closing clickhouse rows", slogx.Error(err),
				slog.String("project_id", projectID))
			telemetry.RecordError(ctx, err)
		}
	}()

	var result []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			slog.ErrorContext(ctx, "clickhouse: query string column scan failed", slogx.Error(err),
				slog.String("project_id", projectID))
			telemetry.RecordError(ctx, err)
			return nil, fmt.Errorf("QueryStringColumn: scan: %w", err)
		}
		result = append(result, id)
	}
	if err := rows.Err(); err != nil {
		slog.ErrorContext(ctx, "clickhouse: query string column iteration failed", slogx.Error(err),
			slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return nil, fmt.Errorf("QueryStringColumn: %w", err)
	}
	return result, nil
}

// QueryFunnel executes a funnel query and returns rows of
// (step_index, event_kind[, breakdown_0..N], value).
func (e *Executor) QueryFunnel(ctx context.Context, projectID string, q FunnelQuery) ([]FunnelRow, error) {
	sql := q.SQL()
	args := q.Args()
	rows, err := e.ch.Query(ctx, sql, args...)
	if err != nil {
		recordQueryError(ctx, "clickhouse: query funnel failed", projectID, sql, len(args), err)
		return nil, fmt.Errorf("QueryFunnel: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "error closing clickhouse rows", slogx.Error(err),
				slog.String("project_id", projectID))
			telemetry.RecordError(ctx, err)
		}
	}()

	var result []FunnelRow
	for rows.Next() {
		row := FunnelRow{Breakdowns: make([]string, q.NumBreakdowns())}
		dest := make([]any, 0, 3+q.NumBreakdowns())
		dest = append(dest, &row.StepIndex, &row.EventKind)
		for i := range row.Breakdowns {
			dest = append(dest, &row.Breakdowns[i])
		}
		dest = append(dest, &row.Value)
		if err := rows.Scan(dest...); err != nil {
			slog.ErrorContext(ctx, "clickhouse: query funnel scan failed", slogx.Error(err),
				slog.String("project_id", projectID))
			telemetry.RecordError(ctx, err)
			return nil, fmt.Errorf("QueryFunnel: scan: %w", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		slog.ErrorContext(ctx, "clickhouse: query funnel iteration failed", slogx.Error(err),
			slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
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

// QueryUserFlow executes a user-flow query and returns rows of (source, target, value).
func (e *Executor) QueryUserFlow(ctx context.Context, projectID string, q UserFlowQuery) ([]UserFlowRow, error) {
	sql := q.SQL()
	args := q.Args()
	rows, err := e.ch.Query(ctx, sql, args...)
	if err != nil {
		recordQueryError(ctx, "clickhouse: query user flow failed", projectID, sql, len(args), err)
		return nil, fmt.Errorf("QueryUserFlow: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "error closing clickhouse rows", slogx.Error(err),
				slog.String("project_id", projectID))
			telemetry.RecordError(ctx, err)
		}
	}()

	var result []UserFlowRow
	for rows.Next() {
		var row UserFlowRow
		// step is emitted as Int32 (toInt32(idx-1)); count(DISTINCT group_key) is
		// UInt64 in ClickHouse, so scan into uint64 then narrow to int64. A
		// distinct-session count for a single edge cannot approach math.MaxInt64, so
		// the conversion never wraps; UserFlowLink.value is int64 (proto, gte=1) and
		// GroupUserFlowResult drops value<=0 as a backstop.
		var value uint64
		if err := rows.Scan(&row.Step, &row.Source, &row.Target, &value); err != nil {
			slog.ErrorContext(ctx, "clickhouse: query user flow scan failed", slogx.Error(err),
				slog.String("project_id", projectID))
			telemetry.RecordError(ctx, err)
			return nil, fmt.Errorf("QueryUserFlow: scan: %w", err)
		}
		row.Value = int64(value)
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		slog.ErrorContext(ctx, "clickhouse: query user flow iteration failed", slogx.Error(err),
			slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return nil, fmt.Errorf("QueryUserFlow: %w", err)
	}
	return result, nil
}

// TopKRow is a single ranked dimension bucket from a top-K query.
type TopKRow struct {
	DimensionValue string
	IsOthers       bool
	Value          float64
}

// QueryTopK executes a top-K query and returns rows of (dim_value, is_others, value)
// in SQL order: top rows metric-descending, the $others bucket (if any) last.
func (e *Executor) QueryTopK(ctx context.Context, projectID string, q TopKQuery) ([]TopKRow, error) {
	sql := q.SQL()
	if sql == "" {
		// Defensive: a zero-value TopKQuery means a builder's (TopKQuery, error)
		// error went unchecked. Fail locally rather than send empty SQL to CH.
		return nil, fmt.Errorf("QueryTopK: empty SQL (unchecked zero-value TopKQuery)")
	}
	args := q.Args()
	rows, err := e.ch.Query(ctx, sql, args...)
	if err != nil {
		recordQueryError(ctx, "clickhouse: query top k failed", projectID, sql, len(args), err)
		return nil, fmt.Errorf("QueryTopK: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "error closing clickhouse rows", slogx.Error(err),
				slog.String("project_id", projectID))
			telemetry.RecordError(ctx, err)
		}
	}()

	var result []TopKRow
	for rows.Next() {
		var row TopKRow
		// is_others is projected as if(..., 0, 1) — a UInt8 in ClickHouse.
		var isOthers uint8
		if err := rows.Scan(&row.DimensionValue, &isOthers, &row.Value); err != nil {
			slog.ErrorContext(ctx, "clickhouse: query top k scan failed", slogx.Error(err),
				slog.String("project_id", projectID))
			telemetry.RecordError(ctx, err)
			return nil, fmt.Errorf("QueryTopK: scan: %w", err)
		}
		row.IsOthers = isOthers != 0
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		slog.ErrorContext(ctx, "clickhouse: query top k iteration failed", slogx.Error(err),
			slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return nil, fmt.Errorf("QueryTopK: %w", err)
	}
	return result, nil
}

// TopKProfileRow is one enrichment row from BuildTopKProfilesQuery.
type TopKProfileRow struct {
	ID         string
	ExternalID string
	Properties map[string]any
}

// QueryTopKProfiles executes the top-K profile enrichment lookup and returns
// rows keyed by profile id. The properties column arrives as a JSON string
// (toJSONString) and is decoded into a plain map.
func (e *Executor) QueryTopKProfiles(ctx context.Context, projectID string, sql string, args []any) (map[string]TopKProfileRow, error) {
	rows, err := e.ch.Query(ctx, sql, args...)
	if err != nil {
		recordQueryError(ctx, "clickhouse: query top k profiles failed", projectID, sql, len(args), err)
		return nil, fmt.Errorf("QueryTopKProfiles: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "error closing clickhouse rows", slogx.Error(err),
				slog.String("project_id", projectID))
			telemetry.RecordError(ctx, err)
		}
	}()

	result := make(map[string]TopKProfileRow)
	for rows.Next() {
		var row TopKProfileRow
		var propertiesJSON string
		if err := rows.Scan(&row.ID, &row.ExternalID, &propertiesJSON); err != nil {
			slog.ErrorContext(ctx, "clickhouse: query top k profiles scan failed", slogx.Error(err),
				slog.String("project_id", projectID))
			telemetry.RecordError(ctx, err)
			return nil, fmt.Errorf("QueryTopKProfiles: scan: %w", err)
		}
		if propertiesJSON != "" && propertiesJSON != "{}" {
			if err := json.Unmarshal([]byte(propertiesJSON), &row.Properties); err != nil {
				slog.ErrorContext(ctx, "clickhouse: query top k profiles properties decode failed", slogx.Error(err),
					slog.String("project_id", projectID), slog.String("profile_id", row.ID))
				telemetry.RecordError(ctx, err)
				return nil, fmt.Errorf("QueryTopKProfiles: decode properties: %w", err)
			}
		}
		result[row.ID] = row
	}
	if err := rows.Err(); err != nil {
		slog.ErrorContext(ctx, "clickhouse: query top k profiles iteration failed", slogx.Error(err),
			slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return nil, fmt.Errorf("QueryTopKProfiles: %w", err)
	}
	return result, nil
}

// QueryFunnelUserEvents executes the array-based funnel query and returns per-user
// event arrays for Go-side step matching and timing computation.
// Columns: (distinct_id, times, step_matches[, breakdown_0..N]).
func (e *Executor) QueryFunnelUserEvents(ctx context.Context, projectID string, q FunnelTimingQuery) ([]FunnelUserEvents, error) {
	sql := q.SQL()
	args := q.Args()
	rows, err := e.ch.Query(ctx, sql, args...)
	if err != nil {
		recordQueryError(ctx, "clickhouse: query funnel user events failed", projectID, sql, len(args), err)
		return nil, fmt.Errorf("QueryFunnelUserEvents: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "error closing clickhouse rows", slogx.Error(err),
				slog.String("project_id", projectID))
			telemetry.RecordError(ctx, err)
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
			slog.ErrorContext(ctx, "clickhouse: query funnel user events scan failed", slogx.Error(err),
				slog.String("project_id", projectID))
			telemetry.RecordError(ctx, err)
			return nil, fmt.Errorf("QueryFunnelUserEvents: scan: %w", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		slog.ErrorContext(ctx, "clickhouse: query funnel user events iteration failed", slogx.Error(err),
			slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
		return nil, fmt.Errorf("QueryFunnelUserEvents: %w", err)
	}
	return result, nil
}

// QueryRetention executes a retention query and returns rows of
// (cohort_time, time, value, cohort_size[, breakdown_0..N]).
func (e *Executor) QueryRetention(ctx context.Context, projectID string, q RetentionQuery) ([]RetentionRow, error) {
	sql := q.SQL()
	args := q.Args()
	rows, err := e.ch.Query(ctx, sql, args...)
	if err != nil {
		recordQueryError(ctx, "clickhouse: query retention failed", projectID, sql, len(args), err)
		return nil, fmt.Errorf("QueryRetention: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.ErrorContext(ctx, "error closing clickhouse rows", slogx.Error(err),
				slog.String("project_id", projectID))
			telemetry.RecordError(ctx, err)
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
			slog.ErrorContext(ctx, "clickhouse: query retention scan failed", slogx.Error(err),
				slog.String("project_id", projectID))
			telemetry.RecordError(ctx, err)
			return nil, fmt.Errorf("QueryRetention: scan: %w", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		slog.ErrorContext(ctx, "clickhouse: query retention iteration failed", slogx.Error(err),
			slog.String("project_id", projectID))
		telemetry.RecordError(ctx, err)
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
//
// When breakdownLimit > 0 and breakdowns are present, only the top N breakdown combinations
// (by total value) are kept per event kind; the rest are merged into a "$others" series.
//
// Validation errors (breakdown/property length mismatch) are logged and recorded against ctx's span.
func GroupSeries(ctx context.Context, rows []TrendRow, properties []string, breakdownLimit int) ([]*insightsv1.TrendSeries, error) {
	var orderedKeys []trendSeriesKey
	entriesByKey := map[trendSeriesKey]*trendSeriesEntry{}

	for _, r := range rows {
		if len(r.Breakdowns) != len(properties) {
			err := fmt.Errorf("row has %d breakdowns but expected %d", len(r.Breakdowns), len(properties))
			slog.ErrorContext(ctx, "GroupSeries: breakdown/property length mismatch", slogx.Error(err))
			telemetry.RecordError(ctx, err)
			return nil, err
		}
		key := trendSeriesKey{eventKind: r.EventKind, breakdown: breakdownKey(r.Breakdowns)}
		if _, ok := entriesByKey[key]; !ok {
			orderedKeys = append(orderedKeys, key)
			bd := make(map[string]string, len(properties))
			for i, prop := range properties {
				bd[prop] = r.Breakdowns[i]
			}
			entriesByKey[key] = &trendSeriesEntry{eventKind: r.EventKind, breakdown: bd}
		}
		entriesByKey[key].total += r.Value
		entriesByKey[key].points = append(entriesByKey[key].points, &insightsv1.DataPoint{
			Time:  timestamppb.New(r.Time),
			Value: proto.Float64(r.Value),
		})
	}

	if breakdownLimit > 0 && len(properties) > 0 {
		orderedKeys, entriesByKey = applyTrendsTopN(orderedKeys, entriesByKey, properties, breakdownLimit)
	}

	series := make([]*insightsv1.TrendSeries, 0, len(orderedKeys))
	for _, k := range orderedKeys {
		e := entriesByKey[k]
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

type trendSeriesKey struct {
	eventKind string
	breakdown string
}

type trendSeriesEntry struct {
	eventKind string
	breakdown map[string]string
	points    []*insightsv1.DataPoint
	total     float64
}

// applyTrendsTopN keeps the top N breakdown combinations per event kind (by total value)
// and merges the rest into a "$others" series, summing points by time bucket.
func applyTrendsTopN(
	orderedKeys []trendSeriesKey,
	entriesByKey map[trendSeriesKey]*trendSeriesEntry,
	properties []string,
	limit int,
) ([]trendSeriesKey, map[trendSeriesKey]*trendSeriesEntry) {
	byEventKind := map[string][]trendSeriesKey{}
	for _, k := range orderedKeys {
		byEventKind[k.eventKind] = append(byEventKind[k.eventKind], k)
	}

	othersBreakdown := make(map[string]string, len(properties))
	for _, prop := range properties {
		othersBreakdown[prop] = "$others"
	}
	othersBreakdownVals := make([]string, len(properties))
	for i := range othersBreakdownVals {
		othersBreakdownVals[i] = "$others"
	}
	othersBreakdownKey := breakdownKey(othersBreakdownVals)

	var newKeys []trendSeriesKey
	eventKinds := make([]string, 0, len(byEventKind))
	for ek := range byEventKind {
		eventKinds = append(eventKinds, ek)
	}
	slices.Sort(eventKinds)

	for _, eventKind := range eventKinds {
		keys := byEventKind[eventKind]
		slices.SortFunc(keys, func(a, b trendSeriesKey) int {
			if c := cmp.Compare(entriesByKey[b].total, entriesByKey[a].total); c != 0 {
				return c
			}
			// Tie-break on breakdown value ascending — matches rollup SQL
			// (sum(cnt) DESC, dim_value ASC) so $others bucketing is identical.
			return cmp.Compare(a.breakdown, b.breakdown)
		})

		if len(keys) <= limit {
			newKeys = append(newKeys, keys...)
			continue
		}

		topKeys := keys[:limit]
		restKeys := keys[limit:]
		newKeys = append(newKeys, topKeys...)

		othersKey := trendSeriesKey{eventKind: eventKind, breakdown: othersBreakdownKey}
		othersEntry := entriesByKey[othersKey]
		if othersEntry == nil {
			othersEntry = &trendSeriesEntry{
				eventKind: eventKind,
				breakdown: othersBreakdown,
			}
			entriesByKey[othersKey] = othersEntry
		}

		for _, rk := range restKeys {
			re := entriesByKey[rk]
			othersEntry.total += re.total
			mergeTrendPoints(othersEntry, re.points)
			delete(entriesByKey, rk)
		}
		newKeys = append(newKeys, othersKey)
	}

	return newKeys, entriesByKey
}

func mergeTrendPoints(dst *trendSeriesEntry, points []*insightsv1.DataPoint) {
	for _, pt := range points {
		merged := false
		for _, op := range dst.points {
			if op.GetTime().AsTime().Equal(pt.GetTime().AsTime()) {
				op.Value = proto.Float64(op.GetValue() + pt.GetValue())
				merged = true
				break
			}
		}
		if !merged {
			dst.points = append(dst.points, &insightsv1.DataPoint{
				Time:  pt.Time,
				Value: proto.Float64(pt.GetValue()),
			})
		}
	}
}

// GroupFunnelSeries groups FunnelRow results into FunnelSeries, keyed by breakdown tuple.
// The properties slice provides the property name for each breakdown dimension.
//
// When breakdownLimit > 0, only the top N breakdown combinations (by step-0 total) are kept;
// the rest are merged into a "$others" series with summed step totals.
//
// Validation errors (breakdown/property length mismatch) are logged and recorded against ctx's span.
func GroupFunnelSeries(ctx context.Context, rows []FunnelRow, properties []string, breakdownLimit int) ([]*insightsv1.FunnelSeries, error) {
	if breakdownLimit > 0 && len(properties) > 0 {
		rows = applyFunnelTopN(rows, properties, breakdownLimit)
	}

	type seriesEntry struct {
		breakdown map[string]string
		steps     []*insightsv1.FunnelStep
	}

	var orderedKeys []string
	entriesByKey := map[string]*seriesEntry{}

	for _, r := range rows {
		if len(r.Breakdowns) != len(properties) {
			err := fmt.Errorf("funnel row has %d breakdowns but expected %d", len(r.Breakdowns), len(properties))
			slog.ErrorContext(ctx, "GroupFunnelSeries: breakdown/property length mismatch", slogx.Error(err))
			telemetry.RecordError(ctx, err)
			return nil, err
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
		step := &insightsv1.FunnelStep{
			EventKind: proto.String(r.EventKind),
			Total:     proto.Float64(r.Value),
		}
		if r.Timing != nil {
			buckets := make([]*insightsv1.DistributionBucket, len(r.Timing.Distribution))
			for i, count := range r.Timing.Distribution {
				bucket := &insightsv1.DistributionBucket{
					Label: proto.String(funnelTimingBuckets[i].label),
					Count: proto.Int64(count),
				}
				if !funnelTimingBuckets[i].openEnded {
					bucket.UpperBound = durationpb.New(funnelTimingBuckets[i].upper)
				}
				buckets[i] = bucket
			}
			step.Timing = &insightsv1.StepTiming{
				Avg:          durationpb.New(r.Timing.Avg),
				Median:       durationpb.New(r.Timing.Median),
				P95:          durationpb.New(r.Timing.P95),
				Distribution: buckets,
			}
		}
		entriesByKey[key].steps = append(entriesByKey[key].steps, step)
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

// applyFunnelTopN rewrites FunnelRow slices: keeps top N breakdown combos by step-0 total,
// merges the rest into "$others" with summed values per step. Timing is dropped for merged rows.
func applyFunnelTopN(rows []FunnelRow, properties []string, limit int) []FunnelRow {
	type bdTotal struct {
		key   string
		total float64
	}
	totals := map[string]float64{}
	for _, r := range rows {
		if r.StepIndex == 0 {
			totals[breakdownKey(r.Breakdowns)] += r.Value
		}
	}
	sorted := make([]bdTotal, 0, len(totals))
	for k, v := range totals {
		sorted = append(sorted, bdTotal{k, v})
	}
	slices.SortFunc(sorted, func(a, b bdTotal) int {
		if c := cmp.Compare(b.total, a.total); c != 0 {
			return c
		}
		return cmp.Compare(a.key, b.key)
	})

	topSet := make(map[string]bool, limit)
	for i := range sorted {
		if i >= limit {
			break
		}
		topSet[sorted[i].key] = true
	}

	othersBreakdowns := make([]string, len(properties))
	for i := range othersBreakdowns {
		othersBreakdowns[i] = "$others"
	}

	// Rewrite: keep top rows as-is, merge rest into $others by step.
	type stepKey struct {
		stepIndex int64
		eventKind string
	}
	othersByStep := map[stepKey]*FunnelRow{}
	var result []FunnelRow
	for _, r := range rows {
		if topSet[breakdownKey(r.Breakdowns)] {
			result = append(result, r)
			continue
		}
		sk := stepKey{r.StepIndex, r.EventKind}
		if existing, ok := othersByStep[sk]; ok {
			existing.Value += r.Value
		} else {
			merged := FunnelRow{
				StepIndex:  r.StepIndex,
				EventKind:  r.EventKind,
				Breakdowns: othersBreakdowns,
				Value:      r.Value,
			}
			othersByStep[sk] = &merged
		}
	}
	for _, r := range othersByStep {
		result = append(result, *r)
	}

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
	return result
}

// GroupRetentionSeries groups RetentionRow results into RetentionSeries, keyed by breakdown tuple.
// Within each series, rows are grouped into cohorts. Insertion order is preserved for both
// series and cohorts.
//
// When breakdownLimit > 0, only the top N breakdown combinations (by total cohort size) are
// kept; the rest are merged into a "$others" series with re-computed retention percentages.
//
// Validation errors (breakdown/property length mismatch) are logged and recorded against ctx's span.
func GroupRetentionSeries(ctx context.Context, rows []RetentionRow, properties []string, breakdownLimit int) ([]*insightsv1.RetentionSeries, error) {
	if breakdownLimit > 0 && len(properties) > 0 {
		rows = applyRetentionTopN(rows, properties, breakdownLimit)
	}

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
			err := fmt.Errorf("retention row has %d breakdowns but expected %d", len(row.Breakdowns), len(properties))
			slog.ErrorContext(ctx, "GroupRetentionSeries: breakdown/property length mismatch", slogx.Error(err))
			telemetry.RecordError(ctx, err)
			return nil, err
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

// applyRetentionTopN keeps the top N breakdown combos by total cohort size, merging the
// rest into "$others". For merged rows, cohort sizes are summed and retention percentages
// are re-computed as weighted averages.
func applyRetentionTopN(rows []RetentionRow, properties []string, limit int) []RetentionRow {
	type bdTotal struct {
		key       string
		cohortSum float64
	}
	totals := map[string]float64{}
	for _, r := range rows {
		totals[breakdownKey(r.Breakdowns)] += r.CohortSize
	}
	sorted := make([]bdTotal, 0, len(totals))
	for k, v := range totals {
		sorted = append(sorted, bdTotal{k, v})
	}
	slices.SortFunc(sorted, func(a, b bdTotal) int {
		if c := cmp.Compare(b.cohortSum, a.cohortSum); c != 0 {
			return c
		}
		return cmp.Compare(a.key, b.key)
	})

	topSet := make(map[string]bool, limit)
	for i := range sorted {
		if i >= limit {
			break
		}
		topSet[sorted[i].key] = true
	}

	othersBreakdowns := make([]string, len(properties))
	for i := range othersBreakdowns {
		othersBreakdowns[i] = "$others"
	}

	type cellKey struct {
		cohortTime time.Time
		time       time.Time
	}
	type cellAgg struct {
		retainedUsers float64
		cohortSize    float64
	}
	othersCells := map[cellKey]*cellAgg{}

	var result []RetentionRow
	for _, r := range rows {
		if topSet[breakdownKey(r.Breakdowns)] {
			result = append(result, r)
			continue
		}
		ck := cellKey{r.CohortTime, r.Time}
		if existing, ok := othersCells[ck]; ok {
			retainedThis := (r.Value / 100.0) * r.CohortSize
			existing.retainedUsers += retainedThis
			existing.cohortSize += r.CohortSize
		} else {
			retainedThis := (r.Value / 100.0) * r.CohortSize
			othersCells[ck] = &cellAgg{retainedUsers: retainedThis, cohortSize: r.CohortSize}
		}
	}

	for ck, agg := range othersCells {
		pct := 0.0
		if agg.cohortSize > 0 {
			pct = (agg.retainedUsers * 100.0) / agg.cohortSize
		}
		result = append(result, RetentionRow{
			CohortTime: ck.cohortTime,
			Time:       ck.time,
			Value:      pct,
			CohortSize: agg.cohortSize,
			Breakdowns: othersBreakdowns,
		})
	}

	return result
}
