package assistant

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
)

func TestSummarizeDraft_AbsentDraftIsStatedNotSkipped(t *testing.T) {
	if out := summarizeDraft(nil); !strings.Contains(out, "none") {
		t.Fatalf("got %q", out)
	}
}

func TestSummarizeDraft_EmptyDraftSaysSo(t *testing.T) {
	out := summarizeDraft(&dashboardsv1.Dashboard{DisplayName: proto.String("New dashboard")})
	if !strings.Contains(out, "no tiles") {
		t.Fatalf("got %q", out)
	}
}

func TestSummarizeDraft_ListsTileWithIDForTargeting(t *testing.T) {
	draft := &dashboardsv1.Dashboard{
		DisplayName: proto.String("Onboarding"),
		Tiles: []*dashboardsv1.DashboardTile{{
			Id:          proto.String("tile_abc"),
			DisplayName: proto.String("Signups"),
			ViewMode:    dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE.Enum(),
			Content: &dashboardsv1.DashboardTile_Insight{Insight: &dashboardsv1.InsightTileContent{
				Spec: &insightsv1.InsightQuerySpec{
					InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
					Events: []*insightsv1.EventQuery{
						{Event: &commonv1.EventFilter{Kind: proto.String("signup")}},
					},
					Breakdowns: []*insightsv1.Breakdown{{Property: proto.String("$country")}},
				},
			}},
		}},
	}

	out := summarizeDraft(draft)
	for _, want := range []string{"tile_abc", "Signups", "signup", "$country", "TRENDS", "LINE"} {
		if !strings.Contains(out, want) {
			t.Fatalf("summary missing %q:\n%s", want, out)
		}
	}
}

func TestSummarizeDraft_MarkdownTileHasNoSpec(t *testing.T) {
	draft := &dashboardsv1.Dashboard{
		DisplayName: proto.String("Notes"),
		Tiles: []*dashboardsv1.DashboardTile{{
			Id:          proto.String("tile_md"),
			DisplayName: proto.String("Context"),
			Content: &dashboardsv1.DashboardTile_Markdown{
				Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("# hello")},
			},
		}},
	}

	out := summarizeDraft(draft)
	if !strings.Contains(out, "markdown") {
		t.Fatalf("got %q", out)
	}
	if strings.Contains(out, "events=") {
		t.Fatalf("markdown tile should not pretend to have a spec: %q", out)
	}
}

func TestSummarizeDraft_InsightTileWithoutSpec(t *testing.T) {
	draft := &dashboardsv1.Dashboard{
		DisplayName: proto.String("d"),
		Tiles: []*dashboardsv1.DashboardTile{{
			Id:      proto.String("tile_x"),
			Content: &dashboardsv1.DashboardTile_Insight{Insight: &dashboardsv1.InsightTileContent{}},
		}},
	}
	if out := summarizeDraft(draft); !strings.Contains(out, "no spec") {
		t.Fatalf("got %q", out)
	}
}

func TestSummarizeDraft_EmptyContentTile(t *testing.T) {
	draft := &dashboardsv1.Dashboard{
		DisplayName: proto.String("d"),
		Tiles:       []*dashboardsv1.DashboardTile{{Id: proto.String("tile_e")}},
	}
	if out := summarizeDraft(draft); !strings.Contains(out, "kind=empty") {
		t.Fatalf("got %q", out)
	}
}
