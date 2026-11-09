package dashboards

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/testutil"
)

const (
	viewLine   = dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE
	viewArea   = dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_AREA
	viewBar    = dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_GROUPED
	viewTable  = dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_TABLE
	viewKPI    = dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_KPI
	viewSankey = dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_SANKEY
	viewMap    = dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_MAP
	viewNone   = dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED
)

func gridPos(x, y, w, h int32) *dashboardsv1.GridPosition {
	return &dashboardsv1.GridPosition{X: proto.Int32(x), Y: proto.Int32(y), W: proto.Int32(w), H: proto.Int32(h)}
}

func eventQuery(kind string) *insightsv1.EventQuery {
	return &insightsv1.EventQuery{Event: &commonv1.EventFilter{Kind: proto.String(kind)}}
}

func trendsSpec(kind string) *insightsv1.InsightQuerySpec {
	return &insightsv1.InsightQuerySpec{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
		Events:      []*insightsv1.EventQuery{eventQuery(kind)},
	}
}

func topKSpec() *insightsv1.InsightQuerySpec {
	return &insightsv1.InsightQuerySpec{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TOP_K.Enum(),
		TopK: &insightsv1.TopKQuery{
			Dimension: insightsv1.TopKQuery_DIMENSION_PROPERTY.Enum(),
			Property:  proto.String("page"),
			Scope:     &commonv1.EventFilter{Kind: proto.String("page_view")},
			Metric:    insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum(),
			Limit:     proto.Int32(10),
		},
	}
}

func funnelSpec() *insightsv1.InsightQuerySpec {
	return &insightsv1.InsightQuerySpec{
		InsightType:      insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
		Events:           []*insightsv1.EventQuery{eventQuery("page_view"), eventQuery("purchase")},
		ConversionWindow: durationpb.New(24 * time.Hour),
	}
}

func markdownInput(name string) UpsertTileInput {
	return UpsertTileInput{Payload: TilePayload{DisplayName: name, Content: MarkdownTile{Body: name + " notes"}}}
}

func newPatchTestService(t *testing.T) (*Service, string) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	db := testutil.SetupPostgres(t)
	return NewService(db.PgRO, db.PgW), seedProject(t, context.Background(), db)
}

// seedBoard saves a dashboard through the app's full-replace Upsert.
func seedBoard(t *testing.T, svc *Service, projectID string, tiles ...UpsertTileInput) DashboardWithTiles {
	t.Helper()
	ctx := context.Background()
	in := UpsertDashboardInput{
		DisplayName:        "Board",
		Description:        "About the board",
		DefaultTimeRange:   commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS,
		DefaultGranularity: insightsv1.Granularity_GRANULARITY_DAY,
		Tiles:              tiles,
	}
	created, err := svc.CreateDashboard(ctx, projectID, in.DisplayName, in.Description, in.DefaultTimeRange, in.DefaultGranularity)
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}
	seeded, err := svc.UpsertDashboard(ctx, projectID, created.ID, in)
	if err != nil {
		t.Fatalf("UpsertDashboard: %v", err)
	}
	return seeded
}

func loadBoard(t *testing.T, svc *Service, projectID, dashboardID string) (dbread.Dashboard, map[string]dbread.DashboardTile) {
	t.Helper()
	got, err := svc.GetDashboard(context.Background(), projectID, dashboardID)
	if err != nil {
		t.Fatalf("GetDashboard: %v", err)
	}
	tiles := make(map[string]dbread.DashboardTile, len(got.Tiles))
	for _, tile := range got.Tiles {
		tiles[tile.ID] = tile
	}
	return got.Dashboard, tiles
}

func tileMessage(t *testing.T, row dbread.DashboardTile) *dashboardsv1.DashboardTile {
	t.Helper()
	msg, err := TileToRPC(context.Background(), row)
	if err != nil {
		t.Fatalf("TileToRPC(%s): %v", row.ID, err)
	}
	return msg
}

func TestPatchDashboard_Create(t *testing.T) {
	svc, projectID := newPatchTestService(t)
	ctx := context.Background()

	res, err := svc.PatchDashboard(ctx, projectID, PatchDashboardInput{
		DisplayName:      proto.String("Growth"),
		DefaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS.Enum(),
		Tiles: []PatchTileInput{
			{DisplayName: proto.String("Signups"), Spec: trendsSpec("signup")},
			{DisplayName: proto.String("Top pages"), Spec: topKSpec()},
			{Markdown: proto.String("Notes")},
			{DisplayName: proto.String("Checkout"), Spec: funnelSpec()},
		},
	})
	if err != nil {
		t.Fatalf("PatchDashboard: %v", err)
	}

	dash, tiles := loadBoard(t, svc, projectID, res.ID)
	if dash.DisplayName != "Growth" || dash.DefaultTimeRange != "TIME_RANGE_PRESET_LAST_7_DAYS" || dash.DefaultGranularity != "GRANULARITY_DAY" {
		t.Errorf("dashboard = %q %s %s, want Growth over the last 7 days by day", dash.DisplayName, dash.DefaultTimeRange, dash.DefaultGranularity)
	}
	if !dash.UpdateTime.Time.Equal(res.UpdateTime) {
		t.Errorf("result update_time = %v, stored %v", res.UpdateTime, dash.UpdateTime.Time)
	}
	// Trends and top K share the first row and its taller height.
	want := []struct {
		view dashboardsv1.DashboardTileViewMode
		pos  *dashboardsv1.GridPosition
	}{
		{viewLine, gridPos(0, 0, 36, 24)},
		{viewTable, gridPos(36, 0, 36, 24)},
		{viewNone, gridPos(0, 24, 24, 16)},
		{viewBar, gridPos(0, 40, 72, 26)},
	}
	if len(res.TileIDs) != len(want) || len(tiles) != len(want) {
		t.Fatalf("got %d tile ids and %d tiles, want %d", len(res.TileIDs), len(tiles), len(want))
	}
	for i, id := range res.TileIDs {
		msg := tileMessage(t, tiles[id])
		if msg.GetViewMode() != want[i].view || !proto.Equal(msg.GetPosition(), want[i].pos) {
			t.Errorf("tile %d: view %v at %v, want %v at %v", i, msg.GetViewMode(), msg.GetPosition(), want[i].view, want[i].pos)
		}
	}
}

func TestPatchDashboard_CreateLeavesNothingWhenATileFails(t *testing.T) {
	svc, projectID := newPatchTestService(t)
	ctx := context.Background()

	_, err := svc.PatchDashboard(ctx, projectID, PatchDashboardInput{
		DisplayName: proto.String("Growth"),
		Tiles: []PatchTileInput{
			{DisplayName: proto.String("Signups"), Markdown: proto.String("one")},
			{DisplayName: proto.String("signups"), Markdown: proto.String("two")},
		},
	})
	if !errors.Is(err, ErrDashboardTileDisplayNameConflict) {
		t.Fatalf("err = %v, want ErrDashboardTileDisplayNameConflict", err)
	}
	dashboards, err := svc.ListDashboards(ctx, projectID)
	if err != nil {
		t.Fatalf("ListDashboards: %v", err)
	}
	if len(dashboards) != 0 {
		t.Errorf("got %d dashboards, want none", len(dashboards))
	}
}

func TestPatchDashboard_EditKeepsWhatItDoesNotName(t *testing.T) {
	svc, projectID := newPatchTestService(t)
	ctx := context.Background()
	seeded := seedBoard(t, svc, projectID,
		UpsertTileInput{Payload: TilePayload{
			DisplayName: "A",
			Content:     InsightTile{Spec: trendsSpec("signup")},
			ViewMode:    viewArea,
			Compare:     dashboardsv1.ComparePeriod_COMPARE_PERIOD_PRIOR,
			Thresholds: []*dashboardsv1.ThresholdRule{{
				Operator: dashboardsv1.ThresholdRule_OPERATOR_GTE.Enum(),
				Value:    proto.Float64(5),
				Tone:     dashboardsv1.ThresholdRule_TONE_GOOD.Enum(),
			}},
			Visualization: &dashboardsv1.VisualizationOptions{YAxisFormat: dashboardsv1.VisualizationOptions_Y_AXIS_FORMAT_COMPACT.Enum()},
			Position:      gridPos(0, 0, 36, 22),
		}},
		UpsertTileInput{Payload: TilePayload{DisplayName: "B", Content: MarkdownTile{Body: "notes"}, Position: gridPos(36, 0, 36, 22)}},
		UpsertTileInput{Payload: TilePayload{DisplayName: "C", Content: InsightTile{Spec: trendsSpec("purchase")}, Position: gridPos(0, 22, 72, 22)}},
	)
	a, b, c := seeded.Tiles[0], seeded.Tiles[1], seeded.Tiles[2]

	res, err := svc.PatchDashboard(ctx, projectID, PatchDashboardInput{
		ID:         seeded.Dashboard.ID,
		UpdateTime: seeded.Dashboard.UpdateTime.Time,
		Tiles: []PatchTileInput{
			{ID: a.ID, DisplayName: proto.String("A, renamed")},
			{Markdown: proto.String("New notes")},
		},
		Remove: []string{c.ID},
	})
	if err != nil {
		t.Fatalf("PatchDashboard: %v", err)
	}

	dash, tiles := loadBoard(t, svc, projectID, seeded.Dashboard.ID)
	if len(tiles) != 3 {
		t.Fatalf("got %d tiles, want 3", len(tiles))
	}
	if _, ok := tiles[c.ID]; ok {
		t.Error("removed tile C is still on the dashboard")
	}
	if !reflect.DeepEqual(tiles[b.ID], b) {
		t.Errorf("untouched tile B changed:\n got %+v\nwant %+v", tiles[b.ID], b)
	}
	before, after := tileMessage(t, a), tileMessage(t, tiles[a.ID])
	before.DisplayName = proto.String("A, renamed")
	before.UpdateTime, after.UpdateTime = nil, nil
	if !proto.Equal(before, after) {
		t.Errorf("renamed tile A lost a field:\n got %v\nwant %v", after, before)
	}
	if len(res.TileIDs) != 2 || res.TileIDs[0] != a.ID {
		t.Fatalf("tile ids = %v, want A's id then the new tile's", res.TileIDs)
	}
	if pos := tileMessage(t, tiles[res.TileIDs[1]]).GetPosition(); !proto.Equal(pos, gridPos(0, 22, 24, 16)) {
		t.Errorf("new tile at %v, want below the lowest tile at (0, 22)", pos)
	}
	if dash.DisplayName != "Board" || dash.Description != "About the board" || dash.DefaultTimeRange != "TIME_RANGE_PRESET_LAST_30_DAYS" {
		t.Errorf("dashboard metadata changed: %+v", dash)
	}
	if !dash.UpdateTime.Time.Equal(res.UpdateTime) {
		t.Errorf("result update_time = %v, stored %v", res.UpdateTime, dash.UpdateTime.Time)
	}
}

func TestPatchDashboard_EditRollsBackWholeWhenATileFails(t *testing.T) {
	svc, projectID := newPatchTestService(t)
	ctx := context.Background()
	seeded := seedBoard(t, svc, projectID, markdownInput("A"), markdownInput("B"), markdownInput("C"))
	_, before := loadBoard(t, svc, projectID, seeded.Dashboard.ID)

	// The removal and the rename run before the new tile's insert clashes with A.
	_, err := svc.PatchDashboard(ctx, projectID, PatchDashboardInput{
		ID:         seeded.Dashboard.ID,
		UpdateTime: seeded.Dashboard.UpdateTime.Time,
		Tiles: []PatchTileInput{
			{ID: seeded.Tiles[1].ID, DisplayName: proto.String("B, renamed")},
			{DisplayName: proto.String("a"), Markdown: proto.String("clashes with A")},
		},
		Remove: []string{seeded.Tiles[2].ID},
	})
	if !errors.Is(err, ErrDashboardTileDisplayNameConflict) {
		t.Fatalf("err = %v, want ErrDashboardTileDisplayNameConflict", err)
	}

	dash, after := loadBoard(t, svc, projectID, seeded.Dashboard.ID)
	if !reflect.DeepEqual(after, before) {
		t.Errorf("tiles changed:\n got %+v\nwant %+v", after, before)
	}
	if !dash.UpdateTime.Time.Equal(seeded.Dashboard.UpdateTime.Time) {
		t.Errorf("update_time moved from %v to %v", seeded.Dashboard.UpdateTime.Time, dash.UpdateTime.Time)
	}
}

func TestPatchDashboard_StaleUpdateTimeRefused(t *testing.T) {
	svc, projectID := newPatchTestService(t)
	ctx := context.Background()
	seeded := seedBoard(t, svc, projectID, markdownInput("A"))

	// Someone renames the dashboard in the app after the caller read it.
	if _, err := svc.UpdateDashboard(ctx, projectID, seeded.Dashboard.ID, UpdateDashboardInput{DisplayName: "Theirs"}); err != nil {
		t.Fatalf("UpdateDashboard: %v", err)
	}
	_, err := svc.PatchDashboard(ctx, projectID, PatchDashboardInput{
		ID:          seeded.Dashboard.ID,
		UpdateTime:  seeded.Dashboard.UpdateTime.Time,
		DisplayName: proto.String("Mine"),
	})
	if !errors.Is(err, ErrDashboardChanged) {
		t.Fatalf("err = %v, want ErrDashboardChanged", err)
	}
	if dash, _ := loadBoard(t, svc, projectID, seeded.Dashboard.ID); dash.DisplayName != "Theirs" {
		t.Errorf("display name = %q, want Theirs", dash.DisplayName)
	}
}

func TestPlaceNewTiles(t *testing.T) {
	spec := func(insightType insightsv1.InsightType) *insightsv1.InsightQuerySpec {
		return &insightsv1.InsightQuerySpec{InsightType: insightType.Enum()}
	}
	insight := func(s *insightsv1.InsightQuerySpec, view dashboardsv1.DashboardTileViewMode) patchTile {
		return patchTile{payload: TilePayload{Content: InsightTile{Spec: s}, ViewMode: view}}
	}
	splitFunnel := spec(insightsv1.InsightType_INSIGHT_TYPE_FUNNEL)
	splitFunnel.Breakdowns = []*insightsv1.Breakdown{{Property: proto.String("$platform")}}

	existing := insight(spec(insightsv1.InsightType_INSIGHT_TYPE_TRENDS), viewLine)
	existing.id = "existing"
	existing.payload.Position = gridPos(0, 0, 72, 20)
	tiles := []patchTile{
		existing,
		insight(spec(insightsv1.InsightType_INSIGHT_TYPE_TRENDS), viewKPI),
		{payload: TilePayload{Content: MarkdownTile{Body: "notes"}}},
		insight(spec(insightsv1.InsightType_INSIGHT_TYPE_TRENDS), viewLine),
		insight(spec(insightsv1.InsightType_INSIGHT_TYPE_TOP_K), viewTable),
		insight(spec(insightsv1.InsightType_INSIGHT_TYPE_MAP), viewMap),
		insight(spec(insightsv1.InsightType_INSIGHT_TYPE_FUNNEL), viewBar),
		insight(splitFunnel, viewBar),
		insight(spec(insightsv1.InsightType_INSIGHT_TYPE_RETENTION), viewTable),
		insight(spec(insightsv1.InsightType_INSIGHT_TYPE_USER_FLOW), viewSankey),
	}
	placeNewTiles(tiles)

	// The doc's table in display columns, six stored units each. The line chart
	// takes the table's height in their shared row.
	want := []*dashboardsv1.GridPosition{
		gridPos(0, 0, 72, 20),
		gridPos(0, 20, 18, 16),
		gridPos(18, 20, 24, 16),
		gridPos(0, 36, 36, 24),
		gridPos(36, 36, 36, 24),
		gridPos(0, 60, 36, 26),
		gridPos(0, 86, 72, 26),
		gridPos(0, 112, 72, 51),
		gridPos(0, 163, 72, 40),
		gridPos(0, 203, 72, 32),
	}
	for i, tile := range tiles {
		if !proto.Equal(tile.payload.Position, want[i]) {
			t.Errorf("tile %d at %v, want %v", i, tile.payload.Position, want[i])
		}
	}
}
