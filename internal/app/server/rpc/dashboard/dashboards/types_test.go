package dashboards

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	coredashboards "github.com/pug-sh/pug/internal/core/dashboards"
	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
)

// TestRenderedDashboardToRPC_CorruptTileDegradesGracefully pins that a tile whose
// stored row can't be re-decoded by the RPC encoder yields a per-tile error_message
// outcome rather than failing the whole QueryDashboard — matching renderInsightTile's
// per-tile handling of the same corruption. Sibling tiles still render.
func TestRenderedDashboardToRPC_CorruptTileDegradesGracefully(t *testing.T) {
	rd := coredashboards.RenderedDashboard{
		Dashboard: dbread.Dashboard{ID: "dash", DisplayName: "D"},
		Tiles: []coredashboards.RenderedTile{
			{
				// Insight row with no stored query: setTileContent fails to encode it.
				Tile:         dbread.DashboardTile{ID: "bad", DashboardID: "dash", Kind: int16(coredashboards.TileKindInsight)},
				ErrorMessage: "insight tile is missing its query",
			},
			{
				Tile: dbread.DashboardTile{ID: "md", DashboardID: "dash", Kind: int16(coredashboards.TileKindMarkdown), MarkdownBody: pgtype.Text{String: "# hi", Valid: true}},
			},
		},
	}

	msg := renderedDashboardToRPC(context.Background(), rd)
	if len(msg.GetTiles()) != 2 {
		t.Fatalf("got %d tiles, want 2", len(msg.GetTiles()))
	}
	bad := msg.GetTiles()[0]
	if bad.GetErrorMessage() == "" {
		t.Errorf("corrupt insight tile: want error_message outcome, got %T", bad.GetOutcome())
	}
	if bad.GetTile().GetId() != "bad" {
		t.Errorf("corrupt tile id = %q, want structural tile preserved (id %q)", bad.GetTile().GetId(), "bad")
	}
	md := msg.GetTiles()[1]
	if md.GetTile().GetMarkdown().GetBody() != "# hi" {
		t.Errorf("sibling markdown tile failed to render: body = %q", md.GetTile().GetMarkdown().GetBody())
	}
}

func TestSetTileContent_InsightHappyPath(t *testing.T) {
	msg := &dashboardsv1.DashboardTile{}
	q := map[string]any{"insightType": "INSIGHT_TYPE_TRENDS"}
	if err := setTileContent(msg, "tile_abc", coredashboards.TileKindInsight, q, "", false); err != nil {
		t.Fatalf("setTileContent insight: %v", err)
	}
	insight, ok := msg.Content.(*dashboardsv1.DashboardTile_Insight)
	if !ok {
		t.Fatalf("Content type = %T, want *DashboardTile_Insight", msg.Content)
	}
	if insight.Insight.GetSpec() == nil {
		t.Fatal("Insight.Spec = nil, want non-nil")
	}
}

func TestSetTileContent_MarkdownHappyPath(t *testing.T) {
	msg := &dashboardsv1.DashboardTile{}
	if err := setTileContent(msg, "tile_abc", coredashboards.TileKindMarkdown, nil, "# heading", true); err != nil {
		t.Fatalf("setTileContent markdown: %v", err)
	}
	markdown, ok := msg.Content.(*dashboardsv1.DashboardTile_Markdown)
	if !ok {
		t.Fatalf("Content type = %T, want *DashboardTile_Markdown", msg.Content)
	}
	if markdown.Markdown.GetBody() != "# heading" {
		t.Errorf("Markdown.Body = %q, want %q", markdown.Markdown.GetBody(), "# heading")
	}
}

// Corruption-guard branches below pin the defensive checks in setTileContent.
// The CHECK constraint on dashboard_tiles guarantees the payload column is
// non-null for each kind, so these branches only fire on data corruption or
// manual DB tinkering. If a future refactor drops a guard, these tests fail
// before the empty/missing payload reaches the client.

func TestSetTileContent_InsightNilQuery(t *testing.T) {
	msg := &dashboardsv1.DashboardTile{}
	err := setTileContent(msg, "tile_abc", coredashboards.TileKindInsight, nil, "", false)
	if err == nil {
		t.Fatal("setTileContent: expected error for nil insight query, got nil")
	}
	if !strings.Contains(err.Error(), "tile_abc") {
		t.Errorf("error %q missing tile ID", err)
	}
	if !strings.Contains(err.Error(), "missing query") {
		t.Errorf("error %q does not mention missing query", err)
	}
}

func TestSetTileContent_InsightEmptyQuery(t *testing.T) {
	msg := &dashboardsv1.DashboardTile{}
	err := setTileContent(msg, "tile_abc", coredashboards.TileKindInsight, map[string]any{}, "", false)
	if err == nil {
		t.Fatal("setTileContent: expected error for empty insight query map, got nil")
	}
	if !strings.Contains(err.Error(), "missing query") {
		t.Errorf("error %q does not mention missing query", err)
	}
}

func TestSetTileContent_MarkdownInvalidBody(t *testing.T) {
	msg := &dashboardsv1.DashboardTile{}
	// markdownValid=false means the DB column was SQL NULL — invalid for kind=markdown.
	err := setTileContent(msg, "tile_abc", coredashboards.TileKindMarkdown, nil, "", false)
	if err == nil {
		t.Fatal("setTileContent: expected error for invalid markdown body, got nil")
	}
	if !strings.Contains(err.Error(), "tile_abc") {
		t.Errorf("error %q missing tile ID", err)
	}
	if !strings.Contains(err.Error(), "missing body") {
		t.Errorf("error %q does not mention missing body", err)
	}
}

func TestSetTileContent_UnknownKind(t *testing.T) {
	msg := &dashboardsv1.DashboardTile{}
	err := setTileContent(msg, "tile_abc", coredashboards.TileKind(99), nil, "", false)
	if err == nil {
		t.Fatal("setTileContent: expected error for unknown kind, got nil")
	}
	if !strings.Contains(err.Error(), "tile_abc") {
		t.Errorf("error %q missing tile ID", err)
	}
	if !strings.Contains(err.Error(), "unknown tile kind") {
		t.Errorf("error %q does not mention unknown kind", err)
	}
}

// MarkdownEmptyBodyValid: markdown_valid=true with body="" is a legitimate
// (if proto-rejected) state — the empty string is a non-NULL value that the
// CHECK constraint accepts. setTileContent must NOT treat valid+empty as
// corruption. (Proto-layer min_len: 1 catches the empty case before any RPC
// reaches the encoder.)
func TestSetTileContent_MarkdownEmptyBodyValid(t *testing.T) {
	msg := &dashboardsv1.DashboardTile{}
	if err := setTileContent(msg, "tile_abc", coredashboards.TileKindMarkdown, nil, "", true); err != nil {
		t.Fatalf("setTileContent markdown empty-but-valid: %v", err)
	}
	markdown, ok := msg.Content.(*dashboardsv1.DashboardTile_Markdown)
	if !ok {
		t.Fatalf("Content type = %T, want *DashboardTile_Markdown", msg.Content)
	}
	if got := markdown.Markdown.GetBody(); got != "" {
		t.Errorf("Markdown.Body = %q, want empty string", got)
	}
}

func TestTileViewModeToRPC_DefaultsInsightToLine(t *testing.T) {
	got := tileViewModeToRPC(coredashboards.TileKindInsight, dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED.String())
	if got != dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE {
		t.Fatalf("tileViewModeToRPC(insight, unspecified) = %v, want LINE", got)
	}
}

func TestTileViewModeToRPC_CoercesMarkdownToUnspecified(t *testing.T) {
	got := tileViewModeToRPC(coredashboards.TileKindMarkdown, dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_GROUPED.String())
	if got != dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED {
		t.Fatalf("tileViewModeToRPC(markdown, bar) = %v, want UNSPECIFIED", got)
	}
}

func TestTileViewModeToRPC_AllInsightModes(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want dashboardsv1.DashboardTileViewMode
	}{
		{"line", dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE.String(), dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE},
		{"area", dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_AREA.String(), dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_AREA},
		{"bar_grouped", dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_GROUPED.String(), dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_GROUPED},
		{"bar_stacked", dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_STACKED.String(), dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_STACKED},
		{"table", dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_TABLE.String(), dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_TABLE},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tileViewModeToRPC(coredashboards.TileKindInsight, tc.raw)
			if got != tc.want {
				t.Fatalf("tileViewModeToRPC(insight, %q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestRoTileToRPC_EmitsViewMode guards that the read-path encoder actually wires
// view_mode onto the proto message (not just that the mapping helper is correct
// in isolation).
func TestRoTileToRPC_EmitsViewMode(t *testing.T) {
	tile := dbread.DashboardTile{
		ID:           "tile_1",
		DashboardID:  "dash_1",
		Kind:         int16(coredashboards.TileKindInsight),
		ViewMode:     dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_AREA.String(),
		InsightQuery: map[string]any{"insightType": "INSIGHT_TYPE_TRENDS"},
		Layouts:      map[string]any{},
	}
	msg, err := roTileToRPC(tile)
	if err != nil {
		t.Fatalf("roTileToRPC: %v", err)
	}
	if msg.GetViewMode() != dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_AREA {
		t.Errorf("ViewMode = %v, want AREA", msg.GetViewMode())
	}
}

func TestWTileToRPC_EmitsViewMode(t *testing.T) {
	tile := dbwrite.DashboardTile{
		ID:           "tile_1",
		DashboardID:  "dash_1",
		Kind:         int16(coredashboards.TileKindInsight),
		ViewMode:     dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_STACKED.String(),
		InsightQuery: map[string]any{"insightType": "INSIGHT_TYPE_TRENDS"},
		Layouts:      map[string]any{},
	}
	msg, err := wTileToRPC(tile)
	if err != nil {
		t.Fatalf("wTileToRPC: %v", err)
	}
	if msg.GetViewMode() != dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_STACKED {
		t.Errorf("ViewMode = %v, want BAR_STACKED", msg.GetViewMode())
	}
}
