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

func TestCreateDashboardTileRequest_RejectsMissingContent(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceCreateTileRequest{
		DashboardId: proto.String("dash_123"),
		DisplayName: proto.String("Signups"),
		// no content oneof set
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for missing content oneof")
	}
}

func TestCreateDashboardTileRequest_RejectsInsightMissingQuery(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceCreateTileRequest{
		DashboardId: proto.String("dash_123"),
		DisplayName: proto.String("Signups"),
		Content: &dashboardsv1.DashboardsServiceCreateTileRequest_Insight{
			Insight: &dashboardsv1.InsightTileContent{}, // query unset
		},
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for insight without query")
	}
}

func TestCreateDashboardTileRequest_AcceptsInsightArm(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceCreateTileRequest{
		DashboardId: proto.String("dash_123"),
		DisplayName: proto.String("Signups"),
		Content: &dashboardsv1.DashboardsServiceCreateTileRequest_Insight{
			Insight: &dashboardsv1.InsightTileContent{Query: validQueryRequest()},
		},
		ViewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE.Enum(),
		DefaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS.Enum(),
		Layouts: []*dashboardsv1.ResponsiveGridLayout{
			{Breakpoint: proto.String("lg"), X: proto.Int32(0), Y: proto.Int32(0), W: proto.Int32(6), H: proto.Int32(4)},
			{Breakpoint: proto.String("md"), X: proto.Int32(0), Y: proto.Int32(4), W: proto.Int32(10), H: proto.Int32(5)},
		},
	}
	if err := protovalidate.Validate(req); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestCreateDashboardTileRequest_RejectsUnknownViewMode(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceCreateTileRequest{
		DashboardId: proto.String("dash_123"),
		DisplayName: proto.String("Signups"),
		Content: &dashboardsv1.DashboardsServiceCreateTileRequest_Insight{
			Insight: &dashboardsv1.InsightTileContent{Query: validQueryRequest()},
		},
		ViewMode: dashboardsv1.DashboardTileViewMode(99).Enum(),
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for unknown view_mode")
	}
}

func TestCreateDashboardTileRequest_RejectsUnknownDefaultTimeRange(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceCreateTileRequest{
		DashboardId: proto.String("dash_123"),
		DisplayName: proto.String("Signups"),
		Content: &dashboardsv1.DashboardsServiceCreateTileRequest_Insight{
			Insight: &dashboardsv1.InsightTileContent{Query: validQueryRequest()},
		},
		DefaultTimeRange: commonv1.TimeRangePreset(99).Enum(),
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for unknown default_time_range")
	}
}

func TestCreateDashboardTileRequest_AcceptsMarkdownArm(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceCreateTileRequest{
		DashboardId: proto.String("dash_123"),
		Content: &dashboardsv1.DashboardsServiceCreateTileRequest_Markdown{
			Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("Hello")},
		},
	}
	if err := protovalidate.Validate(req); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestCreateDashboardTileRequest_RejectsEmptyMarkdownBody(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceCreateTileRequest{
		DashboardId: proto.String("dash_123"),
		Content: &dashboardsv1.DashboardsServiceCreateTileRequest_Markdown{
			Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("")},
		},
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for empty markdown body")
	}
}

func TestCreateDashboardTileRequest_AcceptsMaxMarkdownBody(t *testing.T) {
	body := strings.Repeat("a", 100000)
	req := &dashboardsv1.DashboardsServiceCreateTileRequest{
		DashboardId: proto.String("dash_123"),
		Content: &dashboardsv1.DashboardsServiceCreateTileRequest_Markdown{
			Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String(body)},
		},
	}
	if err := protovalidate.Validate(req); err != nil {
		t.Fatalf("unexpected validation error at exactly 100000 bytes: %v", err)
	}
}

func TestCreateDashboardTileRequest_RejectsOverMaxMarkdownBody(t *testing.T) {
	body := strings.Repeat("a", 100001)
	req := &dashboardsv1.DashboardsServiceCreateTileRequest{
		DashboardId: proto.String("dash_123"),
		Content: &dashboardsv1.DashboardsServiceCreateTileRequest_Markdown{
			Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String(body)},
		},
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for markdown body over 100000 bytes")
	}
}

func TestCreateDashboardTileRequest_RejectsDuplicateBreakpoints(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceCreateTileRequest{
		DashboardId: proto.String("dash_123"),
		DisplayName: proto.String("Signups"),
		Content: &dashboardsv1.DashboardsServiceCreateTileRequest_Insight{
			Insight: &dashboardsv1.InsightTileContent{Query: validQueryRequest()},
		},
		Layouts: []*dashboardsv1.ResponsiveGridLayout{
			{Breakpoint: proto.String("lg"), X: proto.Int32(0), Y: proto.Int32(0), W: proto.Int32(6), H: proto.Int32(4)},
			{Breakpoint: proto.String("lg"), X: proto.Int32(6), Y: proto.Int32(0), W: proto.Int32(6), H: proto.Int32(4)},
		},
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for duplicate breakpoints on insight arm")
	}
}

func TestCreateDashboardTileRequest_RejectsDuplicateBreakpointsOnMarkdown(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceCreateTileRequest{
		DashboardId: proto.String("dash_123"),
		Content: &dashboardsv1.DashboardsServiceCreateTileRequest_Markdown{
			Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("x")},
		},
		Layouts: []*dashboardsv1.ResponsiveGridLayout{
			{Breakpoint: proto.String("lg"), X: proto.Int32(0), Y: proto.Int32(0), W: proto.Int32(6), H: proto.Int32(4)},
			{Breakpoint: proto.String("lg"), X: proto.Int32(6), Y: proto.Int32(0), W: proto.Int32(6), H: proto.Int32(4)},
		},
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for duplicate breakpoints on markdown arm")
	}
}

// ----- ResponsiveGridLayout bounds (T6) ---------------------------------

func TestLayout_RejectsWidthOutOfBounds(t *testing.T) {
	cases := []struct {
		name string
		w    int32
	}{
		{"zero", 0},
		{"over", 25},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := requestWithLayout(&dashboardsv1.ResponsiveGridLayout{
				Breakpoint: proto.String("lg"),
				X:          proto.Int32(0), Y: proto.Int32(0),
				W: proto.Int32(tc.w), H: proto.Int32(4),
			})
			if err := protovalidate.Validate(req); err == nil {
				t.Fatalf("expected validation error for w=%d (allowed range 1..24)", tc.w)
			}
		})
	}
}

func TestLayout_RejectsHeightOutOfBounds(t *testing.T) {
	cases := []struct {
		name string
		h    int32
	}{
		{"zero", 0},
		{"over", 101},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := requestWithLayout(&dashboardsv1.ResponsiveGridLayout{
				Breakpoint: proto.String("lg"),
				X:          proto.Int32(0), Y: proto.Int32(0),
				W: proto.Int32(4), H: proto.Int32(tc.h),
			})
			if err := protovalidate.Validate(req); err == nil {
				t.Fatalf("expected validation error for h=%d (allowed range 1..100)", tc.h)
			}
		})
	}
}

func TestLayout_RejectsNegativePositionAndBounds(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(l *dashboardsv1.ResponsiveGridLayout)
		field string
	}{
		{"x", func(l *dashboardsv1.ResponsiveGridLayout) { l.X = proto.Int32(-1) }, "x"},
		{"y", func(l *dashboardsv1.ResponsiveGridLayout) { l.Y = proto.Int32(-1) }, "y"},
		{"min_w", func(l *dashboardsv1.ResponsiveGridLayout) { l.MinW = proto.Int32(-1) }, "min_w"},
		{"max_w", func(l *dashboardsv1.ResponsiveGridLayout) { l.MaxW = proto.Int32(-1) }, "max_w"},
		{"min_h", func(l *dashboardsv1.ResponsiveGridLayout) { l.MinH = proto.Int32(-1) }, "min_h"},
		{"max_h", func(l *dashboardsv1.ResponsiveGridLayout) { l.MaxH = proto.Int32(-1) }, "max_h"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			layout := &dashboardsv1.ResponsiveGridLayout{
				Breakpoint: proto.String("lg"),
				X:          proto.Int32(0), Y: proto.Int32(0),
				W: proto.Int32(4), H: proto.Int32(4),
			}
			tc.mut(layout)
			if err := protovalidate.Validate(requestWithLayout(layout)); err == nil {
				t.Fatalf("expected validation error for negative %s", tc.field)
			}
		})
	}
}

func TestLayout_RejectsInvalidBreakpointPattern(t *testing.T) {
	// Pattern ^[a-zA-Z0-9_-]+$ — reject space, dot, slash, empty.
	cases := []string{" ", "a b", "a.b", "a/b"}
	for _, bp := range cases {
		t.Run(bp, func(t *testing.T) {
			req := requestWithLayout(&dashboardsv1.ResponsiveGridLayout{
				Breakpoint: proto.String(bp),
				X:          proto.Int32(0), Y: proto.Int32(0),
				W: proto.Int32(4), H: proto.Int32(4),
			})
			if err := protovalidate.Validate(req); err == nil {
				t.Fatalf("expected validation error for breakpoint %q", bp)
			}
		})
	}
}

func TestLayout_RejectsMoreThanEightLayouts(t *testing.T) {
	layouts := make([]*dashboardsv1.ResponsiveGridLayout, 0, 9)
	// Use distinct breakpoint strings to avoid the unique-breakpoints CEL
	// kicking in before max_items — the test must fail specifically on the
	// max_items=8 rule, not on uniqueness.
	for i := 0; i < 9; i++ {
		layouts = append(layouts, &dashboardsv1.ResponsiveGridLayout{
			Breakpoint: proto.String(string(rune('a' + i))),
			X:          proto.Int32(0), Y: proto.Int32(0),
			W: proto.Int32(4), H: proto.Int32(4),
		})
	}
	req := &dashboardsv1.DashboardsServiceCreateTileRequest{
		DashboardId: proto.String("dash_123"),
		Content: &dashboardsv1.DashboardsServiceCreateTileRequest_Markdown{
			Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("x")},
		},
		Layouts: layouts,
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for 9 layouts (max 8)")
	}
}

// requestWithLayout returns a minimal valid CreateTile request carrying the
// single supplied layout. Use this to isolate layout-level validation rules
// from request-level shape requirements.
func requestWithLayout(layout *dashboardsv1.ResponsiveGridLayout) *dashboardsv1.DashboardsServiceCreateTileRequest {
	return &dashboardsv1.DashboardsServiceCreateTileRequest{
		DashboardId: proto.String("dash_123"),
		Content: &dashboardsv1.DashboardsServiceCreateTileRequest_Markdown{
			Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("x")},
		},
		Layouts: []*dashboardsv1.ResponsiveGridLayout{layout},
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

// ----- DashboardsServiceUpdateDisplayNameRequest validation (T7) ---------

func TestUpdateDisplayNameRequest_RejectsMissingID(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceUpdateDisplayNameRequest{
		DisplayName: proto.String("renamed"),
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for missing id")
	}
}

func TestUpdateDisplayNameRequest_RejectsMissingDisplayName(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceUpdateDisplayNameRequest{
		Id: proto.String("dash_123"),
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for missing display_name")
	}
}

func TestUpdateDisplayNameRequest_RejectsOverMaxDisplayName(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceUpdateDisplayNameRequest{
		Id:          proto.String("dash_123"),
		DisplayName: proto.String(strings.Repeat("a", 151)),
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for display_name over 150 chars")
	}
}

func TestUpdateDisplayNameRequest_RejectsOverMaxDescription(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceUpdateDisplayNameRequest{
		Id:          proto.String("dash_123"),
		DisplayName: proto.String("ok"),
		Description: proto.String(strings.Repeat("a", 2001)),
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for description over 2000 chars")
	}
}

// ----- UpdateTileRequest (T8) ------------------------------------------
//
// UpdateTileRequest shares the oneof.required, body bounds, breakpoint
// uniqueness, and layouts max_items rules with CreateTileRequest. These
// tests pin the same rules on the Update variant so a future divergence
// (e.g. dropping oneof.required from one side only) fails loudly.

func TestUpdateTileRequest_RejectsMissingID(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceUpdateTileRequest{
		DashboardId: proto.String("dash_123"),
		Content: &dashboardsv1.DashboardsServiceUpdateTileRequest_Markdown{
			Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("x")},
		},
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for missing id")
	}
}

func TestUpdateTileRequest_RejectsMissingDashboardID(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceUpdateTileRequest{
		Id: proto.String("tile_123"),
		Content: &dashboardsv1.DashboardsServiceUpdateTileRequest_Markdown{
			Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("x")},
		},
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for missing dashboard_id")
	}
}

func TestUpdateTileRequest_RejectsMissingContent(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceUpdateTileRequest{
		Id:          proto.String("tile_123"),
		DashboardId: proto.String("dash_123"),
		// no content oneof set
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for missing content oneof")
	}
}

func TestUpdateTileRequest_RejectsInsightMissingQuery(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceUpdateTileRequest{
		Id:          proto.String("tile_123"),
		DashboardId: proto.String("dash_123"),
		Content: &dashboardsv1.DashboardsServiceUpdateTileRequest_Insight{
			Insight: &dashboardsv1.InsightTileContent{}, // query unset
		},
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for insight without query")
	}
}

func TestUpdateTileRequest_AcceptsInsightArm(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceUpdateTileRequest{
		Id:          proto.String("tile_123"),
		DashboardId: proto.String("dash_123"),
		Content: &dashboardsv1.DashboardsServiceUpdateTileRequest_Insight{
			Insight: &dashboardsv1.InsightTileContent{Query: validQueryRequest()},
		},
		ViewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_AREA.Enum(),
		DefaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_14_DAYS.Enum(),
	}
	if err := protovalidate.Validate(req); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestUpdateTileRequest_RejectsUnknownViewMode(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceUpdateTileRequest{
		Id:          proto.String("tile_123"),
		DashboardId: proto.String("dash_123"),
		Content: &dashboardsv1.DashboardsServiceUpdateTileRequest_Insight{
			Insight: &dashboardsv1.InsightTileContent{Query: validQueryRequest()},
		},
		ViewMode: dashboardsv1.DashboardTileViewMode(99).Enum(),
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for unknown view_mode")
	}
}

func TestUpdateTileRequest_RejectsUnknownDefaultTimeRange(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceUpdateTileRequest{
		Id:          proto.String("tile_123"),
		DashboardId: proto.String("dash_123"),
		Content: &dashboardsv1.DashboardsServiceUpdateTileRequest_Insight{
			Insight: &dashboardsv1.InsightTileContent{Query: validQueryRequest()},
		},
		DefaultTimeRange: commonv1.TimeRangePreset(99).Enum(),
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for unknown default_time_range")
	}
}

func TestUpdateTileRequest_AcceptsMarkdownArm(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceUpdateTileRequest{
		Id:          proto.String("tile_123"),
		DashboardId: proto.String("dash_123"),
		Content: &dashboardsv1.DashboardsServiceUpdateTileRequest_Markdown{
			Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("Hello")},
		},
	}
	if err := protovalidate.Validate(req); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestUpdateTileRequest_RejectsEmptyMarkdownBody(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceUpdateTileRequest{
		Id:          proto.String("tile_123"),
		DashboardId: proto.String("dash_123"),
		Content: &dashboardsv1.DashboardsServiceUpdateTileRequest_Markdown{
			Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("")},
		},
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for empty markdown body")
	}
}

func TestUpdateTileRequest_RejectsOverMaxMarkdownBody(t *testing.T) {
	body := strings.Repeat("a", 100001)
	req := &dashboardsv1.DashboardsServiceUpdateTileRequest{
		Id:          proto.String("tile_123"),
		DashboardId: proto.String("dash_123"),
		Content: &dashboardsv1.DashboardsServiceUpdateTileRequest_Markdown{
			Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String(body)},
		},
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for markdown body over 100000 bytes")
	}
}

func TestUpdateTileRequest_RejectsDuplicateBreakpoints(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceUpdateTileRequest{
		Id:          proto.String("tile_123"),
		DashboardId: proto.String("dash_123"),
		Content: &dashboardsv1.DashboardsServiceUpdateTileRequest_Insight{
			Insight: &dashboardsv1.InsightTileContent{Query: validQueryRequest()},
		},
		Layouts: []*dashboardsv1.ResponsiveGridLayout{
			{Breakpoint: proto.String("lg"), X: proto.Int32(0), Y: proto.Int32(0), W: proto.Int32(6), H: proto.Int32(4)},
			{Breakpoint: proto.String("lg"), X: proto.Int32(6), Y: proto.Int32(0), W: proto.Int32(6), H: proto.Int32(4)},
		},
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for duplicate breakpoints")
	}
}

func TestUpdateTileRequest_RejectsOverMaxDisplayName(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceUpdateTileRequest{
		Id:          proto.String("tile_123"),
		DashboardId: proto.String("dash_123"),
		DisplayName: proto.String(strings.Repeat("a", 151)),
		Content: &dashboardsv1.DashboardsServiceUpdateTileRequest_Markdown{
			Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("x")},
		},
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for display_name over 150 chars")
	}
}

func validQueryRequest() *insightsv1.QueryRequest {
	return &insightsv1.QueryRequest{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
		TimeRange: &commonv1.TimeRange{
			From: timestamppb.New(time.Now().Add(-24 * time.Hour)),
			To:   timestamppb.New(time.Now()),
		},
		Events: []*insightsv1.EventQuery{
			{Event: &commonv1.EventFilter{Kind: proto.String("signup")}},
		},
	}
}
