package dashboards

import (
	"strings"
	"testing"

	coredashboards "github.com/pug-sh/pug/internal/core/dashboards"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
)

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
	if insight.Insight.GetQuery() == nil {
		t.Fatal("Insight.Query = nil, want non-nil")
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

func TestTileDefaultTimeRangeToRPC_DefaultsInsightToLast30Days(t *testing.T) {
	got := tileDefaultTimeRangeToRPC(coredashboards.TileKindInsight, commonv1.TimeRangePreset_TIME_RANGE_PRESET_UNSPECIFIED.String())
	if got != commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS {
		t.Fatalf("tileDefaultTimeRangeToRPC(insight, unspecified) = %v, want LAST_30_DAYS", got)
	}
}

func TestTileDefaultTimeRangeToRPC_CoercesMarkdownToUnspecified(t *testing.T) {
	got := tileDefaultTimeRangeToRPC(coredashboards.TileKindMarkdown, commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_90_DAYS.String())
	if got != commonv1.TimeRangePreset_TIME_RANGE_PRESET_UNSPECIFIED {
		t.Fatalf("tileDefaultTimeRangeToRPC(markdown, last90days) = %v, want UNSPECIFIED", got)
	}
}

func TestTileDefaultTimeRangeToRPC_AllInsightPresets(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want commonv1.TimeRangePreset
	}{
		{"last_1_hour", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_1_HOUR.String(), commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_1_HOUR},
		{"last_6_hours", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_6_HOURS.String(), commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_6_HOURS},
		{"last_24_hours", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_24_HOURS.String(), commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_24_HOURS},
		{"last_7_days", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS.String(), commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS},
		{"last_14_days", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_14_DAYS.String(), commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_14_DAYS},
		{"last_30_days", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS.String(), commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS},
		{"last_90_days", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_90_DAYS.String(), commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_90_DAYS},
		{"last_180_days", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_180_DAYS.String(), commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_180_DAYS},
		{"last_365_days", commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_365_DAYS.String(), commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_365_DAYS},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tileDefaultTimeRangeToRPC(coredashboards.TileKindInsight, tc.raw)
			if got != tc.want {
				t.Fatalf("tileDefaultTimeRangeToRPC(insight, %q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
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

// TestRoTileToRPC_EmitsViewModeAndDefaultTimeRange guards that the read-path
// encoder actually wires view_mode/default_time_range onto the proto message
// (not just that the mapping helpers are correct in isolation).
func TestRoTileToRPC_EmitsViewModeAndDefaultTimeRange(t *testing.T) {
	tile := dbread.DashboardTile{
		ID:               "tile_1",
		DashboardID:      "dash_1",
		Kind:             int16(coredashboards.TileKindInsight),
		ViewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_AREA.String(),
		DefaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_180_DAYS.String(),
		InsightQuery:     map[string]any{"insightType": "INSIGHT_TYPE_TRENDS"},
		Layouts:          map[string]any{},
	}
	msg, err := roTileToRPC(tile)
	if err != nil {
		t.Fatalf("roTileToRPC: %v", err)
	}
	if msg.GetViewMode() != dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_AREA {
		t.Errorf("ViewMode = %v, want AREA", msg.GetViewMode())
	}
	if msg.GetDefaultTimeRange() != commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_180_DAYS {
		t.Errorf("DefaultTimeRange = %v, want LAST_180_DAYS", msg.GetDefaultTimeRange())
	}
}

func TestWTileToRPC_EmitsViewModeAndDefaultTimeRange(t *testing.T) {
	tile := dbwrite.DashboardTile{
		ID:               "tile_1",
		DashboardID:      "dash_1",
		Kind:             int16(coredashboards.TileKindInsight),
		ViewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_STACKED.String(),
		DefaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS.String(),
		InsightQuery:     map[string]any{"insightType": "INSIGHT_TYPE_TRENDS"},
		Layouts:          map[string]any{},
	}
	msg, err := wTileToRPC(tile)
	if err != nil {
		t.Fatalf("wTileToRPC: %v", err)
	}
	if msg.GetViewMode() != dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_STACKED {
		t.Errorf("ViewMode = %v, want BAR_STACKED", msg.GetViewMode())
	}
	if msg.GetDefaultTimeRange() != commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS {
		t.Errorf("DefaultTimeRange = %v, want LAST_7_DAYS", msg.GetDefaultTimeRange())
	}
}
