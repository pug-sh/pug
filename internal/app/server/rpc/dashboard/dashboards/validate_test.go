package dashboards

import (
	"strings"
	"testing"
	"time"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
)

// ----- GridPosition bounds (T6) ------------------------------------------

func TestPosition_RejectsWidthOutOfBounds(t *testing.T) {
	cases := []struct {
		name string
		w    int32
	}{
		{"zero", 0},
		{"over", 25},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := requestWithPosition(&dashboardsv1.GridPosition{
				X: proto.Int32(0), Y: proto.Int32(0),
				W: proto.Int32(tc.w), H: proto.Int32(4),
			})
			if err := protovalidate.Validate(req); err == nil {
				t.Fatalf("expected validation error for w=%d (allowed range 1..24)", tc.w)
			}
		})
	}
}

func TestPosition_RejectsHeightOutOfBounds(t *testing.T) {
	cases := []struct {
		name string
		h    int32
	}{
		{"zero", 0},
		{"over", 101},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := requestWithPosition(&dashboardsv1.GridPosition{
				X: proto.Int32(0), Y: proto.Int32(0),
				W: proto.Int32(4), H: proto.Int32(tc.h),
			})
			if err := protovalidate.Validate(req); err == nil {
				t.Fatalf("expected validation error for h=%d (allowed range 1..100)", tc.h)
			}
		})
	}
}

func TestPosition_RejectsNegativeCoordinates(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(p *dashboardsv1.GridPosition)
		field string
	}{
		{"x", func(p *dashboardsv1.GridPosition) { p.X = proto.Int32(-1) }, "x"},
		{"y", func(p *dashboardsv1.GridPosition) { p.Y = proto.Int32(-1) }, "y"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pos := &dashboardsv1.GridPosition{
				X: proto.Int32(0), Y: proto.Int32(0),
				W: proto.Int32(4), H: proto.Int32(4),
			}
			tc.mut(pos)
			if err := protovalidate.Validate(requestWithPosition(pos)); err == nil {
				t.Fatalf("expected validation error for negative %s", tc.field)
			}
		})
	}
}

func TestPosition_RejectsMissingWidthHeight(t *testing.T) {
	// w/h carry a floor of 1, but under edition-2023 explicit presence a
	// per-field rule fires only when the field is set. A present-but-partial
	// position (x/y given, w/h omitted) would otherwise slip through as a
	// zero-size tile; the message-level CEL requires both w and h whenever a
	// position is set.
	cases := []struct {
		name string
		pos  *dashboardsv1.GridPosition
	}{
		{"x/y only", &dashboardsv1.GridPosition{X: proto.Int32(1), Y: proto.Int32(2)}},
		{"w only", &dashboardsv1.GridPosition{W: proto.Int32(4)}},
		{"h only", &dashboardsv1.GridPosition{H: proto.Int32(4)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := protovalidate.Validate(requestWithPosition(tc.pos)); err == nil {
				t.Fatalf("expected validation error for partial position (%s)", tc.name)
			}
		})
	}
}

func TestPosition_AcceptsBoundaryValues(t *testing.T) {
	// Guards the bounds annotations against an lt-vs-lte slip: the inclusive
	// edges (w 1/24, h 1/100, x/y 0) must validate.
	cases := []struct {
		name string
		pos  *dashboardsv1.GridPosition
	}{
		{"min", &dashboardsv1.GridPosition{X: proto.Int32(0), Y: proto.Int32(0), W: proto.Int32(1), H: proto.Int32(1)}},
		{"max", &dashboardsv1.GridPosition{X: proto.Int32(0), Y: proto.Int32(0), W: proto.Int32(24), H: proto.Int32(100)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := protovalidate.Validate(requestWithPosition(tc.pos)); err != nil {
				t.Fatalf("unexpected validation error at boundary (%s): %v", tc.name, err)
			}
		})
	}
}

// requestWithPosition builds a minimal DashboardTileInput wrapping a single
// position so the TestPosition_* cases can pin GridPosition-level constraints
// (width, height, non-negative coordinates) without duplicating the message
// shape across every test.
func requestWithPosition(position *dashboardsv1.GridPosition) *dashboardsv1.DashboardTileInput {
	return &dashboardsv1.DashboardTileInput{
		Content: &dashboardsv1.DashboardTileInput_Markdown{
			Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("x")},
		},
		Position: position,
	}
}

// ----- DashboardsServiceCreateRequest validation (T7) --------------------

func TestCreateDashboardRequest_RejectsMissingDisplayName(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceCreateRequest{
		Description: proto.String("desc"),
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for missing display_name")
	}
}

func TestCreateDashboardRequest_RejectsOverMaxDisplayName(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceCreateRequest{
		DisplayName: proto.String(strings.Repeat("a", 151)),
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for display_name over 150 chars")
	}
}

func TestCreateDashboardRequest_AcceptsAtMaxDisplayName(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceCreateRequest{
		DisplayName: proto.String(strings.Repeat("a", 150)),
	}
	if err := protovalidate.Validate(req); err != nil {
		t.Fatalf("unexpected validation error at exactly 150 chars: %v", err)
	}
}

func TestCreateDashboardRequest_RejectsOverMaxDescription(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceCreateRequest{
		DisplayName: proto.String("ok"),
		Description: proto.String(strings.Repeat("a", 2001)),
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for description over 2000 chars")
	}
}

// ----- DashboardsServiceUpdateRequest validation (T7) ---------

func TestUpdateRequest_RejectsMissingID(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceUpdateRequest{
		DisplayName: proto.String("renamed"),
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for missing id")
	}
}

func TestUpdateRequest_RejectsMissingDisplayName(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceUpdateRequest{
		Id: proto.String("dash_123"),
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for missing display_name")
	}
}

func TestUpdateRequest_RejectsOverMaxDisplayName(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceUpdateRequest{
		Id:          proto.String("dash_123"),
		DisplayName: proto.String(strings.Repeat("a", 151)),
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for display_name over 150 chars")
	}
}

func TestUpdateRequest_RejectsOverMaxDescription(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceUpdateRequest{
		Id:          proto.String("dash_123"),
		DisplayName: proto.String("ok"),
		Description: proto.String(strings.Repeat("a", 2001)),
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for description over 2000 chars")
	}
}

func TestQueryDashboardRequest_AcceptsOverrides(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceQueryDashboardRequest{
		DashboardId: proto.String("dash_123"),
		TimeRange: &commonv1.TimeRange{
			From: timestamppb.New(time.Now().Add(-24 * time.Hour)),
			To:   timestamppb.New(time.Now()),
		},
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
	}
	if err := protovalidate.Validate(req); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestQueryDashboardRequest_RejectsMissingDashboardID(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceQueryDashboardRequest{}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for missing dashboard_id")
	}
}

func TestRenderedTile_InsightRequiresOutcome(t *testing.T) {
	insightTile := &dashboardsv1.DashboardTile{
		Content: &dashboardsv1.DashboardTile_Insight{
			Insight: &dashboardsv1.InsightTileContent{Spec: validSpec()},
		},
	}
	// Insight rendered tile with no outcome violates rendered_tile.insight_requires_outcome.
	if err := protovalidate.Validate(&dashboardsv1.RenderedTile{Tile: insightTile}); err == nil {
		t.Fatal("expected validation error for insight rendered tile with no outcome")
	}
	// With an error_message it passes.
	withErr := &dashboardsv1.RenderedTile{
		Tile:    insightTile,
		Outcome: &dashboardsv1.RenderedTile_ErrorMessage{ErrorMessage: "query failed"},
	}
	if err := protovalidate.Validate(withErr); err != nil {
		t.Fatalf("unexpected validation error for insight tile with error_message: %v", err)
	}
	// A markdown rendered tile with no outcome is valid.
	markdownTile := &dashboardsv1.RenderedTile{
		Tile: &dashboardsv1.DashboardTile{
			Content: &dashboardsv1.DashboardTile_Markdown{
				Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("x")},
			},
		},
	}
	if err := protovalidate.Validate(markdownTile); err != nil {
		t.Fatalf("unexpected validation error for markdown rendered tile with no outcome: %v", err)
	}
}

func validSpec() *insightsv1.InsightQuerySpec {
	return &insightsv1.InsightQuerySpec{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
		Events: []*insightsv1.EventQuery{
			{Event: &commonv1.EventFilter{Kind: proto.String("signup")}},
		},
	}
}

// ----- Upsert request & DashboardTileInput validation -------------------
//
// These cases pin Upsert request validation at the proto level — protovalidate
// alone rejects the bad shapes before they ever reach the handler.

func validTileInput() *dashboardsv1.DashboardTileInput {
	return &dashboardsv1.DashboardTileInput{
		DisplayName: proto.String("ok"),
		Content: &dashboardsv1.DashboardTileInput_Markdown{
			Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("body")},
		},
	}
}

func TestUpsertRequest_AcceptsValidPayload(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceUpsertRequest{
		Id:          proto.String("dash_123"),
		DisplayName: proto.String("Board"),
		Tiles:       []*dashboardsv1.DashboardTileInput{validTileInput()},
	}
	if err := protovalidate.Validate(req); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestUpsertRequest_RejectsMissingID(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceUpsertRequest{
		DisplayName: proto.String("Board"),
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for missing id")
	}
}

func TestUpsertRequest_RejectsMissingDisplayName(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceUpsertRequest{
		Id: proto.String("dash_123"),
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for missing display_name")
	}
}

func TestUpsertRequest_RejectsOverMaxTiles(t *testing.T) {
	tiles := make([]*dashboardsv1.DashboardTileInput, 101)
	for i := range tiles {
		tiles[i] = validTileInput()
	}
	req := &dashboardsv1.DashboardsServiceUpsertRequest{
		Id:          proto.String("dash_123"),
		DisplayName: proto.String("Board"),
		Tiles:       tiles,
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for 101 tiles")
	}
}

func TestDashboardTileInput_RejectsOverMaxThresholds(t *testing.T) {
	tile := validTileInput()
	tile.Thresholds = make([]*dashboardsv1.ThresholdRule, 6)
	for i := range tile.Thresholds {
		tile.Thresholds[i] = &dashboardsv1.ThresholdRule{
			Operator: dashboardsv1.ThresholdRule_OPERATOR_GT.Enum(),
			Value:    proto.Float64(float64(i)),
			Tone:     dashboardsv1.ThresholdRule_TONE_GOOD.Enum(),
		}
	}
	if err := protovalidate.Validate(tile); err == nil {
		t.Fatal("expected validation error for 6 thresholds (max 5)")
	}
}

func TestDashboardTileInput_AcceptsMaxThresholds(t *testing.T) {
	tile := validTileInput()
	tile.Thresholds = make([]*dashboardsv1.ThresholdRule, 5)
	for i := range tile.Thresholds {
		tile.Thresholds[i] = &dashboardsv1.ThresholdRule{
			Operator: dashboardsv1.ThresholdRule_OPERATOR_GTE.Enum(),
			Value:    proto.Float64(float64(i)),
			Tone:     dashboardsv1.ThresholdRule_TONE_NEUTRAL.Enum(),
		}
	}
	if err := protovalidate.Validate(tile); err != nil {
		t.Fatalf("unexpected validation error at max threshold count: %v", err)
	}
}

func TestThresholdRule_RejectsUndefinedOperator(t *testing.T) {
	rule := &dashboardsv1.ThresholdRule{
		Operator: dashboardsv1.ThresholdRule_Operator(99).Enum(),
		Value:    proto.Float64(1),
		Tone:     dashboardsv1.ThresholdRule_TONE_GOOD.Enum(),
	}
	if err := protovalidate.Validate(rule); err == nil {
		t.Fatal("expected validation error for undefined operator")
	}
}

func TestThresholdRule_RejectsUndefinedTone(t *testing.T) {
	rule := &dashboardsv1.ThresholdRule{
		Operator: dashboardsv1.ThresholdRule_OPERATOR_LT.Enum(),
		Value:    proto.Float64(1),
		Tone:     dashboardsv1.ThresholdRule_Tone(99).Enum(),
	}
	if err := protovalidate.Validate(rule); err == nil {
		t.Fatal("expected validation error for undefined tone")
	}
}

func TestTileHeader_RejectsDisallowedAccentColor(t *testing.T) {
	tile := validTileInput()
	tile.Header = &dashboardsv1.TileHeader{AccentColor: proto.String("neon")}
	if err := protovalidate.Validate(tile); err == nil {
		t.Fatal("expected validation error for accent_color = neon")
	}
}

func TestTileHeader_AcceptsAllowedAccentColors(t *testing.T) {
	allowed := []string{"", "blue", "green", "red", "amber", "purple", "gray"}
	for _, color := range allowed {
		t.Run(color, func(t *testing.T) {
			tile := validTileInput()
			tile.Header = &dashboardsv1.TileHeader{AccentColor: proto.String(color)}
			if err := protovalidate.Validate(tile); err != nil {
				t.Fatalf("unexpected validation error for accent_color = %q: %v", color, err)
			}
		})
	}
}

func TestTileHeader_RejectsOverlongIcon(t *testing.T) {
	tile := validTileInput()
	// 9 bytes — one over the max_len = 8 cap.
	tile.Header = &dashboardsv1.TileHeader{Icon: proto.String(strings.Repeat("a", 9))}
	if err := protovalidate.Validate(tile); err == nil {
		t.Fatal("expected validation error for 9-byte icon")
	}
}

func TestComparePeriod_RejectsUndefined(t *testing.T) {
	tile := validTileInput()
	tile.Compare = dashboardsv1.ComparePeriod(99).Enum()
	if err := protovalidate.Validate(tile); err == nil {
		t.Fatal("expected validation error for undefined compare period")
	}
}

func TestVisualizationOptions_RejectsUndefinedYAxisFormat(t *testing.T) {
	tile := validTileInput()
	tile.Visualization = &dashboardsv1.VisualizationOptions{
		YAxisFormat: dashboardsv1.VisualizationOptions_YAxisFormat(99).Enum(),
	}
	if err := protovalidate.Validate(tile); err == nil {
		t.Fatal("expected validation error for undefined y_axis_format")
	}
}
