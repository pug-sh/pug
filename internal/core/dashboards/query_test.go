package dashboards

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	coreinsights "github.com/pug-sh/pug/internal/core/insights"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// errQueryConn is a driver.Conn whose Query always returns a fixed error,
// simulating e.g. a client disconnect / deadline mid-query (the ClickHouse driver
// surfaces those as context errors). Only Query is exercised; the embedded nil
// Conn satisfies the rest of the interface.
type errQueryConn struct {
	driver.Conn
	queryErr error
}

func (c errQueryConn) Query(_ context.Context, _ string, _ ...any) (driver.Rows, error) {
	return nil, c.queryErr
}

// scalarConn is a driver.Conn whose Query returns a single-row result carrying a
// fixed float64, enough to drive a segmentation tile to a real Result without
// ClickHouse.
type scalarConn struct {
	driver.Conn
	value float64
}

func (c scalarConn) Query(_ context.Context, _ string, _ ...any) (driver.Rows, error) {
	return &scalarRows{value: c.value}, nil
}

type scalarRows struct {
	driver.Rows
	value float64
	done  bool
}

func (r *scalarRows) Next() bool {
	if r.done {
		return false
	}
	r.done = true
	return true
}

func (r *scalarRows) Scan(dest ...any) error {
	if len(dest) > 0 {
		if p, ok := dest[0].(*float64); ok {
			*p = r.value
		}
	}
	return nil
}

func (r *scalarRows) Err() error   { return nil }
func (r *scalarRows) Close() error { return nil }

type userFlowConn struct {
	driver.Conn
	rows [][3]any // source, target, value
}

func (c userFlowConn) Query(_ context.Context, _ string, _ ...any) (driver.Rows, error) {
	return &userFlowRows{rows: c.rows}, nil
}

type userFlowRows struct {
	driver.Rows
	rows [][3]any
	idx  int
}

func (r *userFlowRows) Next() bool {
	return r.idx < len(r.rows)
}

func (r *userFlowRows) Scan(dest ...any) error {
	if len(dest) != 3 {
		return errors.New("userFlowRows: expected 3 scan destinations")
	}
	row := r.rows[r.idx]
	r.idx++
	*dest[0].(*string) = row[0].(string)
	*dest[1].(*string) = row[1].(string)
	// QueryUserFlow scans the count into a uint64 (ClickHouse count(DISTINCT) is
	// UInt64) before narrowing to int64; the mock must match that destination type.
	*dest[2].(*uint64) = uint64(row[2].(int64))
	return nil
}

func (r *userFlowRows) Err() error   { return nil }
func (r *userFlowRows) Close() error { return nil }

func TestDashboardDefaultTimeRangePresetFromDB(t *testing.T) {
	ctx := context.Background()
	if got := DashboardDefaultTimeRangePresetFromDB(ctx, "TIME_RANGE_PRESET_LAST_7_DAYS"); got != commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS {
		t.Fatalf("got %v, want LAST_7_DAYS", got)
	}
	if got := DashboardDefaultTimeRangePresetFromDB(ctx, "unknown"); got != commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS {
		t.Fatalf("unknown got %v, want LAST_30_DAYS", got)
	}
	if got := DashboardDefaultTimeRangePresetFromDB(ctx, ""); got != commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS {
		t.Fatalf("empty got %v, want LAST_30_DAYS", got)
	}
	if got := DashboardDefaultTimeRangePresetFromDB(ctx, "TIME_RANGE_PRESET_UNSPECIFIED"); got != commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS {
		t.Fatalf("unspecified got %v, want LAST_30_DAYS", got)
	}
}

func TestDashboardGranularityFromDB(t *testing.T) {
	ctx := context.Background()
	if got := DashboardGranularityFromDB(ctx, "GRANULARITY_WEEK"); got != insightsv1.Granularity_GRANULARITY_WEEK {
		t.Fatalf("got %v, want WEEK", got)
	}
	if got := DashboardGranularityFromDB(ctx, "unknown"); got != insightsv1.Granularity_GRANULARITY_DAY {
		t.Fatalf("unknown got %v, want DAY", got)
	}
	if got := DashboardGranularityFromDB(ctx, ""); got != insightsv1.Granularity_GRANULARITY_DAY {
		t.Fatalf("empty got %v, want DAY", got)
	}
}

// TestRecordServiceError_SkipsContextErrors pins that a client cancellation/deadline
// is returned unchanged but not logged/recorded (it would manufacture error-rate
// noise), while a genuine failure is both returned and logged at the detecting layer.
func TestRecordServiceError_SkipsContextErrors(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if err := recordServiceError(context.Background(), "boom", context.Canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("context error: got %v, want context.Canceled", err)
	}
	if buf.Len() != 0 {
		t.Errorf("context error must not be logged, got: %s", buf.String())
	}

	buf.Reset()
	sentinel := errors.New("db exploded")
	if err := recordServiceError(context.Background(), "boom", sentinel); !errors.Is(err, sentinel) {
		t.Fatalf("real error: got %v, want sentinel", err)
	}
	if !strings.Contains(buf.String(), "boom") {
		t.Errorf("real error must be logged, got: %q", buf.String())
	}
}

func TestValidAbsoluteTimeRange(t *testing.T) {
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	if validAbsoluteTimeRange(nil) {
		t.Fatal("nil range should be invalid")
	}
	if validAbsoluteTimeRange(&commonv1.TimeRange{
		From: timestamppb.New(now),
		To:   timestamppb.New(now),
	}) {
		t.Fatal("equal bounds should be invalid")
	}
	if !validAbsoluteTimeRange(&commonv1.TimeRange{
		From: timestamppb.New(now.Add(-time.Hour)),
		To:   timestamppb.New(now),
	}) {
		t.Fatal("expected valid range")
	}
}

func TestResolveDashboardTimeRangePreset(t *testing.T) {
	now := time.Date(2026, 5, 23, 15, 30, 0, 0, time.UTC)

	got := ResolveDashboardTimeRangePreset(commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS, nil, now)
	if got.GetFrom().AsTime().After(got.GetTo().AsTime()) {
		t.Fatal("expected from before to")
	}
	if got.GetTo().AsTime() != now {
		t.Fatalf("to = %v, want %v", got.GetTo().AsTime(), now)
	}

	fallback := &commonv1.TimeRange{
		From: timestamppb.New(now.Add(-2 * time.Hour)),
		To:   timestamppb.New(now.Add(-time.Hour)),
	}
	got = ResolveDashboardTimeRangePreset(commonv1.TimeRangePreset_TIME_RANGE_PRESET_UNSPECIFIED, fallback, now)
	if !got.GetFrom().AsTime().Equal(fallback.GetFrom().AsTime()) || !got.GetTo().AsTime().Equal(fallback.GetTo().AsTime()) {
		t.Fatalf("got fallback range %v-%v, want %v-%v", got.GetFrom().AsTime(), got.GetTo().AsTime(), fallback.GetFrom().AsTime(), fallback.GetTo().AsTime())
	}
}

// TestResolveDashboardTimeRangePreset_DayPresetsAreMidnightUTC pins that day-grain
// presets resolve to a midnight-UTC `from` even when the reference clock is in a
// non-UTC zone. The day-keyed rollup's eligibility guard
// (insights.rollupWindowAligned) accepts only midnight-UTC `from`; a midnight-local
// `from` silently disqualifies every default-window tile and forces a raw scan.
func TestResolveDashboardTimeRangePreset_DayPresetsAreMidnightUTC(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tz data unavailable: %v", err)
	}
	now := time.Date(2026, 4, 26, 14, 0, 0, 0, loc) // 14:00 EDT == 18:00 UTC

	for _, preset := range []commonv1.TimeRangePreset{
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS,
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS,
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_365_DAYS,
	} {
		from := ResolveDashboardTimeRangePreset(preset, nil, now).GetFrom().AsTime().UTC()
		midnight := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
		if !from.Equal(midnight) {
			t.Errorf("%v: from = %v, want midnight UTC (%v)", preset, from, midnight)
		}
	}
}

func TestResolveEffectiveWindow_OverrideWins(t *testing.T) {
	now := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	override := &commonv1.TimeRange{From: timestamppb.New(now.Add(-2 * time.Hour)), To: timestamppb.New(now)}
	dash := dbread.Dashboard{DefaultTimeRange: "TIME_RANGE_PRESET_LAST_7_DAYS", DefaultGranularity: "GRANULARITY_WEEK"}

	tr, gran := resolveEffectiveWindow(context.Background(), dash, DashboardQueryOverrides{TimeRange: override, Granularity: insightsv1.Granularity_GRANULARITY_HOUR}, now)
	if tr != override {
		t.Fatal("expected override time range to win")
	}
	if gran != insightsv1.Granularity_GRANULARITY_HOUR {
		t.Fatalf("granularity = %v, want HOUR", gran)
	}
}

func TestResolveEffectiveWindow_FallsBackToDashboardDefault(t *testing.T) {
	now := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	dash := dbread.Dashboard{DefaultTimeRange: "TIME_RANGE_PRESET_LAST_7_DAYS", DefaultGranularity: "GRANULARITY_WEEK"}

	tr, gran := resolveEffectiveWindow(context.Background(), dash, DashboardQueryOverrides{}, now)
	if gran != insightsv1.Granularity_GRANULARITY_WEEK {
		t.Fatalf("granularity = %v, want WEEK", gran)
	}
	wantFrom := startOfDay(now.AddDate(0, 0, -6)) // LAST_7_DAYS = today + 6 prior days
	if !tr.GetFrom().AsTime().Equal(wantFrom) {
		t.Fatalf("from = %v, want %v", tr.GetFrom().AsTime(), wantFrom)
	}
	if !tr.GetTo().AsTime().Equal(now) {
		t.Fatalf("to = %v, want %v", tr.GetTo().AsTime(), now)
	}
}

func TestResolveEffectiveWindow_UnknownDefaultsNormalize(t *testing.T) {
	now := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	dash := dbread.Dashboard{DefaultTimeRange: "", DefaultGranularity: ""}

	tr, gran := resolveEffectiveWindow(context.Background(), dash, DashboardQueryOverrides{}, now)
	if gran != insightsv1.Granularity_GRANULARITY_DAY {
		t.Fatalf("granularity = %v, want DAY", gran)
	}
	wantFrom := startOfDay(now.AddDate(0, 0, -29)) // LAST_30_DAYS fallback = today + 29 prior days
	if !tr.GetFrom().AsTime().Equal(wantFrom) {
		t.Fatalf("from = %v, want %v (LAST_30_DAYS)", tr.GetFrom().AsTime(), wantFrom)
	}
}

func TestRenderDashboard_PreservesOrderAndIncludesMarkdown(t *testing.T) {
	dashboard := DashboardWithTiles{
		Dashboard: dbread.Dashboard{ProjectID: "proj", DefaultTimeRange: "TIME_RANGE_PRESET_LAST_7_DAYS", DefaultGranularity: "GRANULARITY_DAY"},
		Tiles: []dbread.DashboardTile{
			{ID: "markdown", Kind: int16(TileKindMarkdown)},
			{ID: "insight-a", Kind: int16(TileKindInsight)},
			{ID: "insight-b", Kind: int16(TileKindInsight)},
		},
	}

	// nil executor is safe: insight tiles have empty InsightQuery, so each fails
	// before reaching ExecuteQuery.
	rendered, err := RenderDashboard(context.Background(), nil, dashboard, DashboardQueryOverrides{})
	if err != nil {
		t.Fatalf("RenderDashboard returned error: %v", err)
	}

	if len(rendered.Tiles) != 3 {
		t.Fatalf("tiles = %d, want 3 (markdown included)", len(rendered.Tiles))
	}
	gotOrder := []string{rendered.Tiles[0].Tile.ID, rendered.Tiles[1].Tile.ID, rendered.Tiles[2].Tile.ID}
	wantOrder := []string{"markdown", "insight-a", "insight-b"}
	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Fatalf("tile order = %v, want %v", gotOrder, wantOrder)
		}
	}
	if rendered.Tiles[0].Result != nil || rendered.Tiles[0].ErrorMessage != "" {
		t.Fatalf("markdown tile must have no outcome: result=%v err=%q", rendered.Tiles[0].Result, rendered.Tiles[0].ErrorMessage)
	}
	for _, idx := range []int{1, 2} {
		if rendered.Tiles[idx].ErrorMessage == "" {
			t.Fatalf("insight tile %q expected per-tile error message", rendered.Tiles[idx].Tile.ID)
		}
	}
}

func TestRenderDashboard_PerTileRangeCapErrorIsolation(t *testing.T) {
	// Dashboard window GRANULARITY_MINUTE + LAST_30_DAYS violates the MINUTE 6h
	// range cap. The assembled per-tile QueryRequest fails re-validation, so the
	// insight tile surfaces an error_message while the markdown tile and the
	// overall render are unaffected. This guards the per-tile range-cap
	// re-validation path (query.go) that only runs because the window is attached
	// at render time.
	spec := &insightsv1.InsightQuerySpec{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
		Events: []*insightsv1.EventQuery{
			{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
		},
	}
	queryJSON, err := SpecMessageToMap(spec)
	if err != nil {
		t.Fatalf("SpecMessageToMap: %v", err)
	}
	dashboard := DashboardWithTiles{
		Dashboard: dbread.Dashboard{ProjectID: "proj", DefaultTimeRange: "TIME_RANGE_PRESET_LAST_30_DAYS", DefaultGranularity: "GRANULARITY_MINUTE"},
		Tiles: []dbread.DashboardTile{
			{ID: "insight", Kind: int16(TileKindInsight), InsightQuery: queryJSON},
			{ID: "markdown", Kind: int16(TileKindMarkdown)},
		},
	}

	// nil executor is safe: the insight tile fails at protovalidate (range cap)
	// before ExecuteQuery is reached.
	rendered, err := RenderDashboard(context.Background(), nil, dashboard, DashboardQueryOverrides{})
	if err != nil {
		t.Fatalf("RenderDashboard returned error: %v", err)
	}
	if len(rendered.Tiles) != 2 {
		t.Fatalf("tiles = %d, want 2", len(rendered.Tiles))
	}

	insight := rendered.Tiles[0]
	if insight.Result != nil {
		t.Errorf("insight tile should have no result, got %v", insight.Result)
	}
	if !strings.Contains(insight.ErrorMessage, "invalid query parameters") {
		t.Errorf("insight tile error = %q, want range-cap validation error", insight.ErrorMessage)
	}

	markdown := rendered.Tiles[1]
	if markdown.Result != nil || markdown.ErrorMessage != "" {
		t.Errorf("markdown tile must have no outcome: result=%v err=%q", markdown.Result, markdown.ErrorMessage)
	}
}

func TestRenderDashboard_PropagatesContextCancellation(t *testing.T) {
	// A context cancellation / deadline arriving during tile execution must fail
	// the whole RenderDashboard call (so the handler maps it to Canceled/
	// DeadlineExceeded) rather than being masked as a per-tile "query failed" in a
	// 200 response. Guards that the executor wraps the context error, and
	// ExecuteQuery must preserve its identity so renderInsightTile can propagate it.
	spec := &insightsv1.InsightQuerySpec{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION.Enum(),
		Events: []*insightsv1.EventQuery{
			{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
		},
	}
	queryJSON, err := SpecMessageToMap(spec)
	if err != nil {
		t.Fatalf("SpecMessageToMap: %v", err)
	}
	dashboard := DashboardWithTiles{
		Dashboard: dbread.Dashboard{ProjectID: "proj", DefaultTimeRange: "TIME_RANGE_PRESET_LAST_7_DAYS", DefaultGranularity: "GRANULARITY_DAY"},
		Tiles: []dbread.DashboardTile{
			{ID: "insight", Kind: int16(TileKindInsight), InsightQuery: queryJSON},
		},
	}
	executor := coreinsights.NewExecutor(errQueryConn{queryErr: context.Canceled})

	_, err = RenderDashboard(context.Background(), executor, dashboard, DashboardQueryOverrides{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RenderDashboard error = %v, want context.Canceled propagated (not masked as a per-tile failure)", err)
	}
}

func TestRenderDashboard_PropagatesDeadlineExceeded(t *testing.T) {
	// Deadline is the other request-lifecycle error that must fail the whole render
	// (not become a per-tile message), mirroring the cancellation path.
	spec := &insightsv1.InsightQuerySpec{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION.Enum(),
		Events: []*insightsv1.EventQuery{
			{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
		},
	}
	queryJSON, err := SpecMessageToMap(spec)
	if err != nil {
		t.Fatalf("SpecMessageToMap: %v", err)
	}
	dashboard := DashboardWithTiles{
		Dashboard: dbread.Dashboard{ProjectID: "proj", DefaultTimeRange: "TIME_RANGE_PRESET_LAST_7_DAYS", DefaultGranularity: "GRANULARITY_DAY"},
		Tiles:     []dbread.DashboardTile{{ID: "insight", Kind: int16(TileKindInsight), InsightQuery: queryJSON}},
	}
	executor := coreinsights.NewExecutor(errQueryConn{queryErr: context.DeadlineExceeded})

	_, err = RenderDashboard(context.Background(), executor, dashboard, DashboardQueryOverrides{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RenderDashboard error = %v, want context.DeadlineExceeded propagated", err)
	}
}

func TestRenderDashboard_MixedOutcomes(t *testing.T) {
	// One render with three insight tiles exercising every per-tile path at once:
	// a missing-query tile, an invalid-spec tile (segmentation + breakdown violates
	// the proto CEL at the per-tile re-validation), and a healthy tile — interleaved
	// with markdown. Order is preserved and no tile poisons another.
	okSpec := &insightsv1.InsightQuerySpec{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION.Enum(),
		Events: []*insightsv1.EventQuery{
			{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
		},
	}
	okJSON, err := SpecMessageToMap(okSpec)
	if err != nil {
		t.Fatalf("SpecMessageToMap(ok): %v", err)
	}
	// Segmentation does not support breakdowns (proto CEL); this stored spec fails
	// the assembled QueryRequest's re-validation at render time.
	invalidSpec := &insightsv1.InsightQuerySpec{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION.Enum(),
		Events: []*insightsv1.EventQuery{
			{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
		},
		Breakdowns: []*insightsv1.Breakdown{{Property: proto.String("$country")}},
	}
	invalidJSON, err := SpecMessageToMap(invalidSpec)
	if err != nil {
		t.Fatalf("SpecMessageToMap(invalid): %v", err)
	}
	dashboard := DashboardWithTiles{
		Dashboard: dbread.Dashboard{ProjectID: "proj", DefaultTimeRange: "TIME_RANGE_PRESET_LAST_7_DAYS", DefaultGranularity: "GRANULARITY_DAY"},
		Tiles: []dbread.DashboardTile{
			{ID: "missing", Kind: int16(TileKindInsight)},
			{ID: "md", Kind: int16(TileKindMarkdown)},
			{ID: "invalid", Kind: int16(TileKindInsight), InsightQuery: invalidJSON},
			{ID: "ok", Kind: int16(TileKindInsight), InsightQuery: okJSON},
		},
	}
	executor := coreinsights.NewExecutor(scalarConn{value: 7})

	rendered, err := RenderDashboard(context.Background(), executor, dashboard, DashboardQueryOverrides{})
	if err != nil {
		t.Fatalf("RenderDashboard returned error: %v", err)
	}
	if got := []string{rendered.Tiles[0].Tile.ID, rendered.Tiles[1].Tile.ID, rendered.Tiles[2].Tile.ID, rendered.Tiles[3].Tile.ID}; got[0] != "missing" || got[1] != "md" || got[2] != "invalid" || got[3] != "ok" {
		t.Fatalf("tile order = %v, want [missing md invalid ok]", got)
	}

	missing := rendered.Tiles[0]
	if missing.Result != nil || missing.ErrorMessage == "" {
		t.Errorf("missing tile: want error, got result=%v err=%q", missing.Result, missing.ErrorMessage)
	}
	if md := rendered.Tiles[1]; md.Result != nil || md.ErrorMessage != "" {
		t.Errorf("markdown tile: want no outcome, got result=%v err=%q", md.Result, md.ErrorMessage)
	}
	if invalid := rendered.Tiles[2]; invalid.Result != nil || !strings.Contains(invalid.ErrorMessage, "invalid query parameters") {
		t.Errorf("invalid tile: want 'invalid query parameters', got result=%v err=%q", invalid.Result, invalid.ErrorMessage)
	}
	if ok := rendered.Tiles[3]; ok.ErrorMessage != "" || ok.Result.GetSegmentation().GetTotal() != 7 {
		t.Errorf("ok tile: want total=7 no error, got result=%v err=%q", ok.Result, ok.ErrorMessage)
	}
}

func TestRenderDashboard_AllMarkdownNoExecutorNeeded(t *testing.T) {
	dashboard := DashboardWithTiles{
		Dashboard: dbread.Dashboard{ProjectID: "proj", DefaultTimeRange: "TIME_RANGE_PRESET_LAST_7_DAYS", DefaultGranularity: "GRANULARITY_DAY"},
		Tiles: []dbread.DashboardTile{
			{ID: "a", Kind: int16(TileKindMarkdown)},
			{ID: "b", Kind: int16(TileKindMarkdown)},
		},
	}
	// nil executor is safe: no insight tile reaches ExecuteQuery.
	rendered, err := RenderDashboard(context.Background(), nil, dashboard, DashboardQueryOverrides{})
	if err != nil {
		t.Fatalf("RenderDashboard returned error: %v", err)
	}
	if len(rendered.Tiles) != 2 {
		t.Fatalf("tiles = %d, want 2", len(rendered.Tiles))
	}
	for _, tile := range rendered.Tiles {
		if tile.Result != nil || tile.ErrorMessage != "" {
			t.Errorf("markdown tile %q must have no outcome", tile.Tile.ID)
		}
	}
}

func TestRenderDashboard_EmptyDashboard(t *testing.T) {
	dashboard := DashboardWithTiles{
		Dashboard: dbread.Dashboard{ProjectID: "proj", DefaultTimeRange: "TIME_RANGE_PRESET_LAST_7_DAYS", DefaultGranularity: "GRANULARITY_DAY"},
	}
	rendered, err := RenderDashboard(context.Background(), nil, dashboard, DashboardQueryOverrides{})
	if err != nil {
		t.Fatalf("RenderDashboard returned error: %v", err)
	}
	if len(rendered.Tiles) != 0 {
		t.Fatalf("tiles = %d, want 0", len(rendered.Tiles))
	}
}

func TestRenderDashboard_PartialSuccessIsolatesFailure(t *testing.T) {
	// One tile fails (missing query) while a sibling succeeds with a real Result in
	// the same render — proves per-tile isolation doesn't poison healthy siblings.
	// Guards partial-success isolation.
	spec := &insightsv1.InsightQuerySpec{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION.Enum(),
		Events: []*insightsv1.EventQuery{
			{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}, Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum()},
		},
	}
	queryJSON, err := SpecMessageToMap(spec)
	if err != nil {
		t.Fatalf("SpecMessageToMap: %v", err)
	}
	dashboard := DashboardWithTiles{
		Dashboard: dbread.Dashboard{ProjectID: "proj", DefaultTimeRange: "TIME_RANGE_PRESET_LAST_7_DAYS", DefaultGranularity: "GRANULARITY_DAY"},
		Tiles: []dbread.DashboardTile{
			{ID: "broken", Kind: int16(TileKindInsight)}, // empty InsightQuery → per-tile error
			{ID: "ok", Kind: int16(TileKindInsight), InsightQuery: queryJSON},
		},
	}
	executor := coreinsights.NewExecutor(scalarConn{value: 42})

	rendered, err := RenderDashboard(context.Background(), executor, dashboard, DashboardQueryOverrides{})
	if err != nil {
		t.Fatalf("RenderDashboard returned error: %v", err)
	}
	broken := rendered.Tiles[0]
	if broken.Result != nil || broken.ErrorMessage == "" {
		t.Errorf("broken tile: want error message, got result=%v err=%q", broken.Result, broken.ErrorMessage)
	}
	ok := rendered.Tiles[1]
	if ok.ErrorMessage != "" {
		t.Errorf("ok tile: unexpected error %q", ok.ErrorMessage)
	}
	if ok.Result.GetSegmentation().GetTotal() != 42 {
		t.Errorf("ok tile total = %v, want 42 (sibling failure must not poison it)", ok.Result.GetSegmentation().GetTotal())
	}
}

func TestRenderDashboard_UserFlowTile(t *testing.T) {
	spec := &insightsv1.InsightQuerySpec{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_USER_FLOW.Enum(),
		UserFlow:    &insightsv1.UserFlowQuery{},
	}
	queryJSON, err := SpecMessageToMap(spec)
	if err != nil {
		t.Fatalf("SpecMessageToMap: %v", err)
	}
	dashboard := DashboardWithTiles{
		Dashboard: dbread.Dashboard{ProjectID: "proj", DefaultTimeRange: "TIME_RANGE_PRESET_LAST_7_DAYS", DefaultGranularity: "GRANULARITY_DAY"},
		Tiles: []dbread.DashboardTile{
			{ID: "uf", Kind: int16(TileKindInsight), InsightQuery: queryJSON},
		},
	}
	executor := coreinsights.NewExecutor(userFlowConn{rows: [][3]any{
		{"login", "dashboard", int64(2)},
		{"login", "logout", int64(1)},
	}})

	rendered, err := RenderDashboard(context.Background(), executor, dashboard, DashboardQueryOverrides{})
	if err != nil {
		t.Fatalf("RenderDashboard returned error: %v", err)
	}
	if len(rendered.Tiles) != 1 {
		t.Fatalf("tiles = %d, want 1", len(rendered.Tiles))
	}
	tile := rendered.Tiles[0]
	if tile.ErrorMessage != "" {
		t.Fatalf("unexpected error: %q", tile.ErrorMessage)
	}
	result := tile.Result.GetUserFlow()
	if result == nil {
		t.Fatal("expected UserFlow result")
	}
	if len(result.GetLinks()) != 2 {
		t.Fatalf("links = %d, want 2", len(result.GetLinks()))
	}
}
