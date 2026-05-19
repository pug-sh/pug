package projects_test

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/pug-sh/pug/internal/core/projects"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
)

func TestEncodeTileContent_Insight(t *testing.T) {
	q := &insightsv1.QueryRequest{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
		Events: []*insightsv1.EventQuery{
			{Event: &commonv1.EventFilter{Kind: proto.String("signup")}},
		},
	}

	enc, err := projects.InsightTile{Query: q}.Encode()
	if err != nil {
		t.Fatalf("EncodeTileContent insight: %v", err)
	}
	if enc.Kind != projects.TileKindInsight {
		t.Errorf("Kind = %d, want %d", enc.Kind, projects.TileKindInsight)
	}
	if enc.InsightQuery == nil {
		t.Error("InsightQuery = nil, want non-nil")
	}
	if enc.MarkdownBody.Valid {
		t.Error("MarkdownBody.Valid = true, want false (SQL NULL)")
	}
	if enc.InsightQuery["insightType"] != "INSIGHT_TYPE_TRENDS" {
		t.Errorf("InsightQuery[insightType] = %v, want INSIGHT_TYPE_TRENDS", enc.InsightQuery["insightType"])
	}
}

func TestEncodeTileContent_Markdown(t *testing.T) {
	body := "# Heading\n\nSome text with an image: ![alt](https://example.com/img.png)"

	enc, err := projects.MarkdownTile{Body: body}.Encode()
	if err != nil {
		t.Fatalf("EncodeTileContent markdown: %v", err)
	}
	if enc.Kind != projects.TileKindMarkdown {
		t.Errorf("Kind = %d, want %d", enc.Kind, projects.TileKindMarkdown)
	}
	if enc.InsightQuery != nil {
		t.Error("InsightQuery != nil, want nil (SQL NULL)")
	}
	if !enc.MarkdownBody.Valid {
		t.Fatal("MarkdownBody.Valid = false, want true")
	}
	if enc.MarkdownBody.String != body {
		t.Errorf("MarkdownBody.String = %q, want %q", enc.MarkdownBody.String, body)
	}
}

func TestLayoutsRoundTrip_AllFields(t *testing.T) {
	// Pin all nine numeric fields plus static=true through a full round-trip
	// LayoutsToMap → MapToLayouts. A typo in any of the JSON key spellings
	// (minW, maxW, minH, maxH) on either side would silently drop the value
	// to zero — this test exercises every field so such a typo fails loudly.
	// Also pins the deterministic breakpoint ordering (sort.Strings).
	in := []*dashboardsv1.ResponsiveGridLayout{
		{
			Breakpoint: proto.String("md"),
			X:          proto.Int32(1),
			Y:          proto.Int32(2),
			W:          proto.Int32(3),
			H:          proto.Int32(4),
			MinW:       proto.Int32(5),
			MaxW:       proto.Int32(6),
			MinH:       proto.Int32(7),
			MaxH:       proto.Int32(8),
			Static:     proto.Bool(false),
		},
		{
			Breakpoint: proto.String("lg"),
			X:          proto.Int32(9),
			Y:          proto.Int32(10),
			W:          proto.Int32(11),
			H:          proto.Int32(12),
			MinW:       proto.Int32(13),
			MaxW:       proto.Int32(14),
			MinH:       proto.Int32(15),
			MaxH:       proto.Int32(16),
			Static:     proto.Bool(true),
		},
	}

	encoded := projects.LayoutsToMap(in)
	out, err := projects.MapToLayouts(encoded)
	if err != nil {
		t.Fatalf("MapToLayouts: %v", err)
	}

	// Sorted ascending by breakpoint string: "lg" < "md".
	want := []string{"lg", "md"}
	got := []string{out[0].GetBreakpoint(), out[1].GetBreakpoint()}
	if got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("breakpoint order = %v, want %v", got, want)
	}

	lg := out[0]
	if lg.GetX() != 9 || lg.GetY() != 10 || lg.GetW() != 11 || lg.GetH() != 12 {
		t.Errorf("lg x/y/w/h = %d/%d/%d/%d, want 9/10/11/12", lg.GetX(), lg.GetY(), lg.GetW(), lg.GetH())
	}
	if lg.GetMinW() != 13 || lg.GetMaxW() != 14 || lg.GetMinH() != 15 || lg.GetMaxH() != 16 {
		t.Errorf("lg minW/maxW/minH/maxH = %d/%d/%d/%d, want 13/14/15/16",
			lg.GetMinW(), lg.GetMaxW(), lg.GetMinH(), lg.GetMaxH())
	}
	if !lg.GetStatic() {
		t.Error("lg.Static = false, want true")
	}

	md := out[1]
	if md.GetX() != 1 || md.GetY() != 2 || md.GetW() != 3 || md.GetH() != 4 {
		t.Errorf("md x/y/w/h = %d/%d/%d/%d, want 1/2/3/4", md.GetX(), md.GetY(), md.GetW(), md.GetH())
	}
	if md.GetMinW() != 5 || md.GetMaxW() != 6 || md.GetMinH() != 7 || md.GetMaxH() != 8 {
		t.Errorf("md minW/maxW/minH/maxH = %d/%d/%d/%d, want 5/6/7/8",
			md.GetMinW(), md.GetMaxW(), md.GetMinH(), md.GetMaxH())
	}
	if md.GetStatic() {
		t.Error("md.Static = true, want false")
	}
}

func TestEncodeTileContent_EmptyMarkdown(t *testing.T) {
	// Empty markdown body is unreachable via the RPC path (proto enforces min_len: 1),
	// but EncodeTileContent must encode the empty string verbatim with Valid=true.
	// MarkdownTile{Body: ""} is structurally distinct from no markdown content at all
	// (which is unrepresentable in the sealed TileContent type system).
	enc, err := projects.MarkdownTile{Body: ""}.Encode()
	if err != nil {
		t.Fatalf("MarkdownTile.Encode empty body: %v", err)
	}
	if enc.Kind != projects.TileKindMarkdown {
		t.Errorf("Kind = %d, want %d", enc.Kind, projects.TileKindMarkdown)
	}
	if enc.InsightQuery != nil {
		t.Errorf("InsightQuery = %v, want nil", enc.InsightQuery)
	}
	if !enc.MarkdownBody.Valid {
		t.Fatal("MarkdownBody.Valid = false, want true")
	}
	if enc.MarkdownBody.String != "" {
		t.Errorf("MarkdownBody.String = %q, want empty string", enc.MarkdownBody.String)
	}
}
