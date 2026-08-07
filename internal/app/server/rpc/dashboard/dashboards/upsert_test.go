package dashboards

import (
	"context"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/rs/xid"
	"google.golang.org/protobuf/proto"

	"github.com/pug-sh/pug/internal/apperr"
	coredashboards "github.com/pug-sh/pug/internal/core/dashboards"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/testutil"
)

func TestHandler_Upsert_Unauthenticated(t *testing.T) {
	s := &Server{}
	_, err := s.Upsert(context.Background(), connect.NewRequest(&dashboardsv1.DashboardsServiceUpsertRequest{
		Id:          proto.String("x"),
		DisplayName: proto.String("y"),
	}))
	assertCode(t, err, connect.CodeUnauthenticated)
}

func TestHandler_Upsert_DashboardNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	s, projectID, _ := newIntegrationServer(t)

	_, err := s.Upsert(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceUpsertRequest{
		Id:          proto.String("nonexistent_dashboard"),
		DisplayName: proto.String("x"),
	}))
	assertCode(t, err, connect.CodeNotFound)
	assertReason(t, err, apperr.ReasonDashboardNotFound)
}

func TestHandler_Upsert_TileIDNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	s, projectID, svc := newIntegrationServer(t)
	ctx := context.Background()

	dash, err := svc.CreateDashboard(ctx, projectID, "Board", "",
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS, insightsv1.Granularity_GRANULARITY_DAY)
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}

	_, err = s.Upsert(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceUpsertRequest{
		Id:          proto.String(dash.ID),
		DisplayName: proto.String("Board"),
		Tiles: []*dashboardsv1.DashboardTileInput{
			{
				Id: proto.String("ghost_tile_id_00000"),
				Content: &dashboardsv1.DashboardTileInput_Markdown{
					Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("body")},
				},
			},
		},
	}))
	assertCode(t, err, connect.CodeNotFound)
	assertReason(t, err, apperr.ReasonDashboardTileNotFound)
}

// TestHandler_Upsert_InsertUpdateDeleteRoundTrip exercises the core reconcile:
// one insert (empty id), one update (matching id), one implicit delete (an
// existing id omitted from the request). Verifies response tiles are in
// request order and a follow-up Get sees the same state.
func TestHandler_Upsert_InsertUpdateDeleteRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	s, projectID, svc := newIntegrationServer(t)
	ctx := context.Background()

	dash, err := svc.CreateDashboard(ctx, projectID, "Board", "",
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS, insightsv1.Granularity_GRANULARITY_DAY)
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}

	// Seed: two tiles. One will be updated, one will be deleted.
	seeded := upsertSeedTiles(t, s, projectID, dash.ID,
		seedTile{name: "keep", body: "before"},
		seedTile{name: "drop", body: "doomed"},
	)
	keep, toDelete := seeded[0], seeded[1]

	resp, err := s.Upsert(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceUpsertRequest{
		Id:                 proto.String(dash.ID),
		DisplayName:        proto.String("Board v2"),
		Description:        proto.String("updated"),
		DefaultTimeRange:   commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS.Enum(),
		DefaultGranularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
		Tiles: []*dashboardsv1.DashboardTileInput{
			// Update existing
			{
				Id:          proto.String(keep),
				DisplayName: proto.String("keep"),
				Content: &dashboardsv1.DashboardTileInput_Markdown{
					Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("after")},
				},
			},
			// New (empty id) — placed second so request order is observable.
			{
				DisplayName: proto.String("brand new"),
				Content: &dashboardsv1.DashboardTileInput_Markdown{
					Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("hello")},
				},
			},
			// `toDelete` is omitted → should be deleted.
		},
	}))
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	tiles := resp.Msg.GetDashboard().GetTiles()
	if len(tiles) != 2 {
		t.Fatalf("response tiles = %d, want 2 (insert+update; deleted one absent)", len(tiles))
	}
	if tiles[0].GetId() != keep {
		t.Errorf("response order: first tile id = %q, want %q (update should be first in request order)", tiles[0].GetId(), keep)
	}
	if tiles[0].GetMarkdown().GetBody() != "after" {
		t.Errorf("updated body = %q, want %q", tiles[0].GetMarkdown().GetBody(), "after")
	}
	if tiles[1].GetId() == "" {
		t.Errorf("inserted tile missing server-assigned id")
	}
	if tiles[1].GetId() == toDelete || tiles[1].GetId() == keep {
		t.Errorf("inserted tile id %q collided with existing", tiles[1].GetId())
	}
	if tiles[1].GetMarkdown().GetBody() != "hello" {
		t.Errorf("inserted body = %q, want %q", tiles[1].GetMarkdown().GetBody(), "hello")
	}

	// Dashboard metadata was rewritten.
	if got := resp.Msg.GetDashboard().GetDisplayName(); got != "Board v2" {
		t.Errorf("display_name = %q, want %q", got, "Board v2")
	}
	if got := resp.Msg.GetDashboard().GetDefaultTimeRange(); got != commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS {
		t.Errorf("default_time_range = %v, want LAST_7_DAYS", got)
	}

	// A follow-up Get must see the same set. (Read goes through a different
	// query than the Upsert reload; this pins both paths to the same state.)
	getResp, err := s.Get(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceGetRequest{
		Id: proto.String(dash.ID),
	}))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	gotIDs := map[string]bool{}
	for _, t := range getResp.Msg.GetDashboard().GetTiles() {
		gotIDs[t.GetId()] = true
	}
	if !gotIDs[keep] {
		t.Errorf("Get missing kept tile %q", keep)
	}
	if gotIDs[toDelete] {
		t.Errorf("Get still sees deleted tile %q", toDelete)
	}
	if len(gotIDs) != 2 {
		t.Errorf("Get tile count = %d, want 2", len(gotIDs))
	}
}

// TestHandler_Upsert_HashShortCircuit verifies that an Upsert whose tile
// payload matches the stored hash does NOT bump the tile's update_time —
// the SQL WHERE payload_hash <> $1 should match zero rows.
func TestHandler_Upsert_HashShortCircuit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	s, projectID, svc := newIntegrationServer(t)
	ctx := context.Background()

	dash, err := svc.CreateDashboard(ctx, projectID, "Board", "",
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS, insightsv1.Granularity_GRANULARITY_DAY)
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}
	tileID := upsertSeedTiles(t, s, projectID, dash.ID, seedTile{name: "card", body: "body"})[0]

	buildReq := func() *dashboardsv1.DashboardsServiceUpsertRequest {
		return &dashboardsv1.DashboardsServiceUpsertRequest{
			Id:          proto.String(dash.ID),
			DisplayName: proto.String("Board"),
			Tiles: []*dashboardsv1.DashboardTileInput{
				{
					Id:          proto.String(tileID),
					DisplayName: proto.String("card"),
					Content: &dashboardsv1.DashboardTileInput_Markdown{
						Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("body")},
					},
				},
			},
		}
	}

	// First Upsert with content matching the seeded tile. After this, the
	// stored payload_hash matches what this exact payload would produce.
	resp1, err := s.Upsert(authCtx(projectID), connect.NewRequest(buildReq()))
	if err != nil {
		t.Fatalf("Upsert #1: %v", err)
	}
	t1 := resp1.Msg.GetDashboard().GetTiles()[0].GetUpdateTime().AsTime()

	// Second Upsert with identical payload should be a no-op at the row level.
	// Build a fresh request rather than reusing the same pointer — defensive
	// against future Connect versions that might mutate the message.
	resp2, err := s.Upsert(authCtx(projectID), connect.NewRequest(buildReq()))
	if err != nil {
		t.Fatalf("Upsert #2: %v", err)
	}
	t2 := resp2.Msg.GetDashboard().GetTiles()[0].GetUpdateTime().AsTime()

	if !t1.Equal(t2) {
		t.Errorf("tile update_time bumped on byte-equal upsert: %v -> %v", t1, t2)
	}
}

// TestHandler_Upsert_EmptyTilesClearsDashboard verifies the omit-all case:
// a dashboard with tiles upserted with an empty tile list ends up with zero
// tiles and no error.
func TestHandler_Upsert_EmptyTilesClearsDashboard(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	s, projectID, svc := newIntegrationServer(t)
	ctx := context.Background()

	dash, err := svc.CreateDashboard(ctx, projectID, "Board", "",
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS, insightsv1.Granularity_GRANULARITY_DAY)
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}
	upsertSeedTiles(t, s, projectID, dash.ID,
		seedTile{name: "first", body: "x"},
		seedTile{name: "second", body: "y"},
	)

	resp, err := s.Upsert(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceUpsertRequest{
		Id:          proto.String(dash.ID),
		DisplayName: proto.String("Board"),
		Tiles:       nil,
	}))
	if err != nil {
		t.Fatalf("Upsert with empty tiles: %v", err)
	}
	if got := resp.Msg.GetDashboard().GetTiles(); len(got) != 0 {
		t.Errorf("response tiles = %d, want 0", len(got))
	}

	getResp, err := s.Get(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceGetRequest{
		Id: proto.String(dash.ID),
	}))
	if err != nil {
		t.Fatalf("Get after empty upsert: %v", err)
	}
	if got := getResp.Msg.GetDashboard().GetTiles(); len(got) != 0 {
		t.Errorf("post-upsert Get tiles = %d, want 0", len(got))
	}
}

// TestHandler_Upsert_OnlyChangedTileBumpsUpdateTime is the granular cousin of
// the hash short-circuit test: Upsert two tiles, change only one, and assert
// the unchanged tile's update_time is preserved while the changed one's bumps.
func TestHandler_Upsert_OnlyChangedTileBumpsUpdateTime(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	s, projectID, svc := newIntegrationServer(t)
	ctx := context.Background()

	dash, err := svc.CreateDashboard(ctx, projectID, "Board", "",
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS, insightsv1.Granularity_GRANULARITY_DAY)
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}
	seeded := upsertSeedTiles(t, s, projectID, dash.ID,
		seedTile{name: "stable", body: "still"},
		seedTile{name: "changed", body: "old body"},
	)
	idStable, idChanged := seeded[0], seeded[1]

	initial, err := s.Get(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceGetRequest{
		Id: proto.String(dash.ID),
	}))
	if err != nil {
		t.Fatalf("initial Get: %v", err)
	}
	initialTimes := map[string]time.Time{}
	for _, tile := range initial.Msg.GetDashboard().GetTiles() {
		initialTimes[tile.GetId()] = tile.GetUpdateTime().AsTime()
	}

	resp, err := s.Upsert(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceUpsertRequest{
		Id:          proto.String(dash.ID),
		DisplayName: proto.String("Board"),
		Tiles: []*dashboardsv1.DashboardTileInput{
			// Identical payload — hash matches stored, no UPDATE.
			{
				Id:          proto.String(idStable),
				DisplayName: proto.String("stable"),
				Content: &dashboardsv1.DashboardTileInput_Markdown{
					Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("still")},
				},
			},
			// New body — hash differs, UPDATE fires.
			{
				Id:          proto.String(idChanged),
				DisplayName: proto.String("changed"),
				Content: &dashboardsv1.DashboardTileInput_Markdown{
					Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("new body")},
				},
			},
		},
	}))
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	gotTimes := map[string]time.Time{}
	for _, tile := range resp.Msg.GetDashboard().GetTiles() {
		gotTimes[tile.GetId()] = tile.GetUpdateTime().AsTime()
	}
	if !initialTimes[idStable].Equal(gotTimes[idStable]) {
		t.Errorf("stable tile update_time bumped: %v -> %v", initialTimes[idStable], gotTimes[idStable])
	}
	if gotTimes[idChanged].Equal(initialTimes[idChanged]) {
		t.Errorf("changed tile update_time did not bump: %v -> %v", initialTimes[idChanged], gotTimes[idChanged])
	}
}

// TestHandler_Upsert_DuplicateNameRollsBack verifies that a mid-transaction
// failure (here, a unique-violation on display_name during an INSERT) undoes
// updates that happened earlier in the same Upsert call. To force the
// collision under the delete-first ordering, the new tile claims the name
// of a tile that's also being kept (so the conflict can't be resolved by an
// earlier DELETE).
func TestHandler_Upsert_DuplicateNameRollsBack(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	s, projectID, svc := newIntegrationServer(t)
	ctx := context.Background()

	dash, err := svc.CreateDashboard(ctx, projectID, "Board", "",
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS, insightsv1.Granularity_GRANULARITY_DAY)
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}
	seeded := upsertSeedTiles(t, s, projectID, dash.ID,
		seedTile{name: "alpha", body: "original alpha"},
		seedTile{name: "beta", body: "original beta"},
	)
	alpha, beta := seeded[0], seeded[1]

	// Keep both seeded tiles in the request (alpha gets a body change so its
	// update is observable on rollback). The third slot is a new tile claiming
	// alpha's name — collides with the kept alpha row mid-tx.
	_, err = s.Upsert(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceUpsertRequest{
		Id:          proto.String(dash.ID),
		DisplayName: proto.String("Board"),
		Tiles: []*dashboardsv1.DashboardTileInput{
			{
				Id:          proto.String(alpha),
				DisplayName: proto.String("alpha"),
				Content: &dashboardsv1.DashboardTileInput_Markdown{
					Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("MUTATED ALPHA")},
				},
			},
			{
				Id:          proto.String(beta),
				DisplayName: proto.String("beta"),
				Content: &dashboardsv1.DashboardTileInput_Markdown{
					Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("original beta")},
				},
			},
			{
				// New tile, display_name collides with the kept alpha row.
				DisplayName: proto.String("alpha"),
				Content: &dashboardsv1.DashboardTileInput_Markdown{
					Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("conflicting")},
				},
			},
		},
	}))
	assertCode(t, err, connect.CodeAlreadyExists)
	assertReason(t, err, apperr.ReasonDashboardTileNameConflict)

	getResp, err := s.Get(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceGetRequest{
		Id: proto.String(dash.ID),
	}))
	if err != nil {
		t.Fatalf("Get after rolled-back upsert: %v", err)
	}
	var alphaBody string
	for _, tile := range getResp.Msg.GetDashboard().GetTiles() {
		if tile.GetId() == alpha {
			alphaBody = tile.GetMarkdown().GetBody()
		}
	}
	if alphaBody != "original alpha" {
		t.Errorf("alpha body after rollback = %q, want %q (tx rollback did not undo the update)", alphaBody, "original alpha")
	}
	if got := len(getResp.Msg.GetDashboard().GetTiles()); got != 2 {
		t.Errorf("tile count after rollback = %d, want 2 (no inserts should have stuck)", got)
	}
}

// TestHandler_Upsert_KpiUnspecifiedComparePersists pins that KPI tiles don't
// require a compare period — COMPARE_PERIOD_UNSPECIFIED must round-trip rather
// than being rejected or normalized away into something else.
func TestHandler_Upsert_KpiUnspecifiedComparePersists(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	s, projectID, svc := newIntegrationServer(t)
	ctx := context.Background()

	dash, err := svc.CreateDashboard(ctx, projectID, "KPIs", "",
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS, insightsv1.Granularity_GRANULARITY_DAY)
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}

	resp, err := s.Upsert(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceUpsertRequest{
		Id:          proto.String(dash.ID),
		DisplayName: proto.String("KPIs"),
		Tiles: []*dashboardsv1.DashboardTileInput{
			{
				DisplayName: proto.String("Signups"),
				Content: &dashboardsv1.DashboardTileInput_Insight{
					Insight: &dashboardsv1.InsightTileContent{Spec: &insightsv1.InsightQuerySpec{
						InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
						Events: []*insightsv1.EventQuery{
							{
								Event:       &commonv1.EventFilter{Kind: proto.String("signup")},
								Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum(),
							},
						},
					}},
				},
				ViewMode: dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_KPI.Enum(),
				Compare:  dashboardsv1.ComparePeriod_COMPARE_PERIOD_UNSPECIFIED.Enum(),
			},
		},
	}))
	if err != nil {
		t.Fatalf("Upsert KPI tile: %v", err)
	}
	tiles := resp.Msg.GetDashboard().GetTiles()
	if len(tiles) != 1 {
		t.Fatalf("response tiles = %d, want 1", len(tiles))
	}
	if got := tiles[0].GetViewMode(); got != dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_KPI {
		t.Errorf("view_mode = %v, want KPI", got)
	}
	if got := tiles[0].GetCompare(); got != dashboardsv1.ComparePeriod_COMPARE_PERIOD_UNSPECIFIED {
		t.Errorf("compare = %v, want COMPARE_PERIOD_UNSPECIFIED", got)
	}
}

// TestHandler_Upsert_CrossProjectIsolation verifies that project B cannot
// touch project A's dashboard via Upsert. Each WHERE clause in the upsert
// path (`GetDashboardByIDAndProjectID`, `UpsertDashboardTileUpdate`,
// `DeleteDashboardTilesNotIn`, `UpsertDashboardMetadata`) is scoped by
// project_id; this test pins that the pre-load rejects the cross-project
// access before any write runs.
func TestHandler_Upsert_CrossProjectIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	s, projectA, projectB, svc := newIntegrationServerTwoProjects(t)
	ctx := context.Background()

	dash, err := svc.CreateDashboard(ctx, projectA, "Owned by A", "",
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS, insightsv1.Granularity_GRANULARITY_DAY)
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}
	upsertSeedTilesWithName(t, s, projectA, dash.ID, "Owned by A",
		seedTile{name: "secret", body: "private body"})

	// Project B targets A's dashboard ID. The pre-load must fail with NotFound.
	_, err = s.Upsert(authCtx(projectB), connect.NewRequest(&dashboardsv1.DashboardsServiceUpsertRequest{
		Id:          proto.String(dash.ID),
		DisplayName: proto.String("Hijacked"),
		Tiles: []*dashboardsv1.DashboardTileInput{
			{
				DisplayName: proto.String("intruder"),
				Content: &dashboardsv1.DashboardTileInput_Markdown{
					Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("evil")},
				},
			},
		},
	}))
	assertCode(t, err, connect.CodeNotFound)
	assertReason(t, err, apperr.ReasonDashboardNotFound)

	// Project A still sees its tile intact.
	getResp, err := s.Get(authCtx(projectA), connect.NewRequest(&dashboardsv1.DashboardsServiceGetRequest{
		Id: proto.String(dash.ID),
	}))
	if err != nil {
		t.Fatalf("Get from project A: %v", err)
	}
	tiles := getResp.Msg.GetDashboard().GetTiles()
	if len(tiles) != 1 || tiles[0].GetDisplayName() != "secret" {
		t.Errorf("project A's tiles were modified by project B: %+v", tiles)
	}
	if got := getResp.Msg.GetDashboard().GetDisplayName(); got != "Owned by A" {
		t.Errorf("project A's display_name was modified by project B: %q", got)
	}
}

// TestHandler_Upsert_TileCustomizationRoundTrip pins that every per-tile
// customization field — Compare (non-default), Thresholds (populated), Header
// (populated), Visualization (populated), Position (populated) — survives
// Upsert→Postgres→Get with byte-identical content. This is the headline feature
// of the PR and the only test that exercises non-zero values across the whole
// pipeline.
func TestHandler_Upsert_TileCustomizationRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	s, projectID, svc := newIntegrationServer(t)
	ctx := context.Background()

	dash, err := svc.CreateDashboard(ctx, projectID, "KPIs", "",
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS, insightsv1.Granularity_GRANULARITY_DAY)
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}

	want := &dashboardsv1.DashboardTileInput{
		DisplayName: proto.String("Revenue"),
		Description: proto.String("daily revenue KPI"),
		Content: &dashboardsv1.DashboardTileInput_Insight{
			Insight: &dashboardsv1.InsightTileContent{Spec: &insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
				Events: []*insightsv1.EventQuery{
					{
						Event:       &commonv1.EventFilter{Kind: proto.String("purchase")},
						Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum(),
					},
				},
			}},
		},
		ViewMode: dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_KPI.Enum(),
		Compare:  dashboardsv1.ComparePeriod_COMPARE_PERIOD_PRIOR.Enum(),
		Thresholds: []*dashboardsv1.ThresholdRule{
			{
				Operator: dashboardsv1.ThresholdRule_OPERATOR_GTE.Enum(),
				Value:    proto.Float64(1000),
				Tone:     dashboardsv1.ThresholdRule_TONE_GOOD.Enum(),
			},
			{
				Operator: dashboardsv1.ThresholdRule_OPERATOR_LT.Enum(),
				Value:    proto.Float64(500),
				Tone:     dashboardsv1.ThresholdRule_TONE_BAD.Enum(),
			},
		},
		Header: &dashboardsv1.TileHeader{
			Icon:        proto.String("💰"),
			AccentColor: proto.String("green"),
			HideTitle:   proto.Bool(true),
		},
		Visualization: &dashboardsv1.VisualizationOptions{
			YAxisFormat:  dashboardsv1.VisualizationOptions_Y_AXIS_FORMAT_PERCENT.Enum(),
			LogScale:     proto.Bool(true),
			HideLegend:   proto.Bool(true),
			ZeroBaseline: proto.Bool(false),
		},
		Position: &dashboardsv1.GridPosition{
			X: proto.Int32(2), Y: proto.Int32(4),
			W: proto.Int32(6), H: proto.Int32(3),
		},
	}

	upsertResp, err := s.Upsert(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceUpsertRequest{
		Id:          proto.String(dash.ID),
		DisplayName: proto.String("KPIs"),
		Tiles:       []*dashboardsv1.DashboardTileInput{want},
	}))
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if len(upsertResp.Msg.GetDashboard().GetTiles()) != 1 {
		t.Fatalf("upsert tiles = %d, want 1", len(upsertResp.Msg.GetDashboard().GetTiles()))
	}

	// Round-trip via a fresh Get (not the Upsert response) to ensure the read
	// path is what we're testing.
	getResp, err := s.Get(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceGetRequest{
		Id: proto.String(dash.ID),
	}))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got := getResp.Msg.GetDashboard().GetTiles()[0]

	// DashboardTileInput and DashboardTile share field shapes; compare
	// customization fields directly since their getters resolve identically.
	if got.GetCompare() != want.GetCompare() {
		t.Errorf("Compare: got %v, want %v", got.GetCompare(), want.GetCompare())
	}
	if got.GetViewMode() != want.GetViewMode() {
		t.Errorf("ViewMode: got %v, want %v", got.GetViewMode(), want.GetViewMode())
	}
	if len(got.GetThresholds()) != len(want.GetThresholds()) {
		t.Errorf("Thresholds len: got %d, want %d", len(got.GetThresholds()), len(want.GetThresholds()))
	} else {
		for i, gt := range got.GetThresholds() {
			if !proto.Equal(gt, want.GetThresholds()[i]) {
				t.Errorf("Thresholds[%d]: got %v, want %v", i, gt, want.GetThresholds()[i])
			}
		}
	}
	if !proto.Equal(got.GetHeader(), want.GetHeader()) {
		t.Errorf("Header: got %v, want %v", got.GetHeader(), want.GetHeader())
	}
	if !proto.Equal(got.GetVisualization(), want.GetVisualization()) {
		t.Errorf("Visualization: got %v, want %v", got.GetVisualization(), want.GetVisualization())
	}
	if !proto.Equal(got.GetPosition(), want.GetPosition()) {
		t.Errorf("Position: got %v, want %v", got.GetPosition(), want.GetPosition())
	}
}

// TestHandler_Upsert_CaseInsensitiveDisplayNameConflict pins that the partial
// unique index on `lower(display_name)` is honored. If a future migration
// drops the `lower()` wrapper, "Notes" and "notes" would no longer collide and
// this test would break — surfacing the regression at CI.
func TestHandler_Upsert_CaseInsensitiveDisplayNameConflict(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	s, projectID, svc := newIntegrationServer(t)
	ctx := context.Background()

	dash, err := svc.CreateDashboard(ctx, projectID, "Board", "",
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS, insightsv1.Granularity_GRANULARITY_DAY)
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}

	_, err = s.Upsert(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceUpsertRequest{
		Id:          proto.String(dash.ID),
		DisplayName: proto.String("Board"),
		Tiles: []*dashboardsv1.DashboardTileInput{
			{
				DisplayName: proto.String("Notes"),
				Content: &dashboardsv1.DashboardTileInput_Markdown{
					Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("uppercase")},
				},
			},
			{
				DisplayName: proto.String("notes"),
				Content: &dashboardsv1.DashboardTileInput_Markdown{
					Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("lowercase")},
				},
			},
		},
	}))
	assertCode(t, err, connect.CodeAlreadyExists)
	assertReason(t, err, apperr.ReasonDashboardTileNameConflict)
}

// TestHandler_Upsert_RenameIntoExistingConflict pins that renaming an existing
// tile to collide with another existing tile's name fails the whole Upsert.
func TestHandler_Upsert_RenameIntoExistingConflict(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	s, projectID, svc := newIntegrationServer(t)
	ctx := context.Background()

	dash, err := svc.CreateDashboard(ctx, projectID, "Board", "",
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS, insightsv1.Granularity_GRANULARITY_DAY)
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}
	seeded := upsertSeedTiles(t, s, projectID, dash.ID,
		seedTile{name: "alpha", body: "a"},
		seedTile{name: "beta", body: "b"},
	)
	alpha := seeded[0]

	_, err = s.Upsert(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceUpsertRequest{
		Id:          proto.String(dash.ID),
		DisplayName: proto.String("Board"),
		Tiles: []*dashboardsv1.DashboardTileInput{
			{
				Id:          proto.String(alpha),
				DisplayName: proto.String("beta"), // rename into beta's name
				Content: &dashboardsv1.DashboardTileInput_Markdown{
					Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("a")},
				},
			},
			{
				DisplayName: proto.String("beta"),
				Content: &dashboardsv1.DashboardTileInput_Markdown{
					Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("b")},
				},
			},
		},
	}))
	assertCode(t, err, connect.CodeAlreadyExists)
	assertReason(t, err, apperr.ReasonDashboardTileNameConflict)
}

// TestHandler_Upsert_TileKindSwap pins that an existing markdown tile can be
// updated in place to an insight tile (and vice-versa) — the
// dashboard_tiles_kind_payload CHECK constraint must allow both directions.
// If a future SQL refactor leaves a stale insight_query / markdown_body
// alongside the new kind, this test surfaces the violation.
func TestHandler_Upsert_TileKindSwap(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	s, projectID, svc := newIntegrationServer(t)
	ctx := context.Background()

	dash, err := svc.CreateDashboard(ctx, projectID, "Board", "",
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS, insightsv1.Granularity_GRANULARITY_DAY)
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}
	tileID := upsertSeedTiles(t, s, projectID, dash.ID, seedTile{name: "swap", body: "as markdown"})[0]

	// Swap markdown → insight.
	resp, err := s.Upsert(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceUpsertRequest{
		Id:          proto.String(dash.ID),
		DisplayName: proto.String("Board"),
		Tiles: []*dashboardsv1.DashboardTileInput{
			{
				Id:          proto.String(tileID),
				DisplayName: proto.String("swap"),
				Content: &dashboardsv1.DashboardTileInput_Insight{
					Insight: &dashboardsv1.InsightTileContent{Spec: &insightsv1.InsightQuerySpec{
						InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
						Events: []*insightsv1.EventQuery{{
							Event:       &commonv1.EventFilter{Kind: proto.String("page_view")},
							Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum(),
						}},
					}},
				},
				ViewMode: dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE.Enum(),
			},
		},
	}))
	if err != nil {
		t.Fatalf("markdown→insight upsert: %v", err)
	}
	tile := resp.Msg.GetDashboard().GetTiles()[0]
	if tile.GetInsight() == nil {
		t.Errorf("after swap, expected insight tile, got %v", tile.GetContent())
	}
	if tile.GetMarkdown() != nil {
		t.Errorf("after swap, markdown content still present: %v", tile.GetMarkdown())
	}

	// Swap insight → markdown.
	resp, err = s.Upsert(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceUpsertRequest{
		Id:          proto.String(dash.ID),
		DisplayName: proto.String("Board"),
		Tiles: []*dashboardsv1.DashboardTileInput{
			{
				Id:          proto.String(tileID),
				DisplayName: proto.String("swap"),
				Content: &dashboardsv1.DashboardTileInput_Markdown{
					Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("back to markdown")},
				},
			},
		},
	}))
	if err != nil {
		t.Fatalf("insight→markdown upsert: %v", err)
	}
	tile = resp.Msg.GetDashboard().GetTiles()[0]
	if tile.GetMarkdown().GetBody() != "back to markdown" {
		t.Errorf("after swap-back, body = %q, want %q", tile.GetMarkdown().GetBody(), "back to markdown")
	}
	if tile.GetInsight() != nil {
		t.Errorf("after swap-back, insight still present: %v", tile.GetInsight())
	}
}

// TestHandler_Upsert_ReplaceAllTilesWithNewSet pins the corner of the omit-to-
// delete contract where every existing tile is omitted AND new tiles are
// inserted in the same call: 3 existing → 2 new (empty-id) → all 3 originals
// deleted, exactly the 2 new tiles present.
func TestHandler_Upsert_ReplaceAllTilesWithNewSet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	s, projectID, svc := newIntegrationServer(t)
	ctx := context.Background()

	dash, err := svc.CreateDashboard(ctx, projectID, "Board", "",
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS, insightsv1.Granularity_GRANULARITY_DAY)
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}
	original := upsertSeedTiles(t, s, projectID, dash.ID,
		seedTile{name: "one", body: "1"},
		seedTile{name: "two", body: "2"},
		seedTile{name: "three", body: "3"},
	)

	resp, err := s.Upsert(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceUpsertRequest{
		Id:          proto.String(dash.ID),
		DisplayName: proto.String("Board"),
		Tiles: []*dashboardsv1.DashboardTileInput{
			{
				DisplayName: proto.String("fresh-one"),
				Content: &dashboardsv1.DashboardTileInput_Markdown{
					Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("brand new")},
				},
			},
			{
				DisplayName: proto.String("fresh-two"),
				Content: &dashboardsv1.DashboardTileInput_Markdown{
					Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("also new")},
				},
			},
		},
	}))
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	tiles := resp.Msg.GetDashboard().GetTiles()
	if len(tiles) != 2 {
		t.Fatalf("response tiles = %d, want 2", len(tiles))
	}
	for _, tile := range tiles {
		for _, origID := range original {
			if tile.GetId() == origID {
				t.Errorf("new tile got an old id: %q", tile.GetId())
			}
		}
	}
	if tiles[0].GetDisplayName() != "fresh-one" || tiles[1].GetDisplayName() != "fresh-two" {
		t.Errorf("response order broken: %s, %s", tiles[0].GetDisplayName(), tiles[1].GetDisplayName())
	}
}

// TestHandler_Upsert_InsightSpecUpdate pins that updating an insight tile's
// spec (e.g., changing the event filter) produces a new payload_hash, fires
// the UPDATE, and the new spec is what Get returns. All other Upsert tests
// use markdown tiles; this is the only one exercising insight-spec changes
// through the RPC path.
func TestHandler_Upsert_InsightSpecUpdate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	s, projectID, svc := newIntegrationServer(t)
	ctx := context.Background()

	dash, err := svc.CreateDashboard(ctx, projectID, "Insights", "",
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS, insightsv1.Granularity_GRANULARITY_DAY)
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}

	build := func(eventKind string) *dashboardsv1.DashboardTileInput {
		return &dashboardsv1.DashboardTileInput{
			DisplayName: proto.String("Trends"),
			Content: &dashboardsv1.DashboardTileInput_Insight{
				Insight: &dashboardsv1.InsightTileContent{Spec: &insightsv1.InsightQuerySpec{
					InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
					Events: []*insightsv1.EventQuery{{
						Event:       &commonv1.EventFilter{Kind: proto.String(eventKind)},
						Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum(),
					}},
				}},
			},
		}
	}

	resp1, err := s.Upsert(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceUpsertRequest{
		Id:          proto.String(dash.ID),
		DisplayName: proto.String("Insights"),
		Tiles:       []*dashboardsv1.DashboardTileInput{build("signup")},
	}))
	if err != nil {
		t.Fatalf("Upsert #1: %v", err)
	}
	tileID := resp1.Msg.GetDashboard().GetTiles()[0].GetId()
	t1 := resp1.Msg.GetDashboard().GetTiles()[0].GetUpdateTime().AsTime()

	// Re-upsert with a different event kind — hash must change, update_time
	// must bump, returned spec must reflect the new event.
	updated := build("purchase")
	updated.Id = proto.String(tileID)
	resp2, err := s.Upsert(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceUpsertRequest{
		Id:          proto.String(dash.ID),
		DisplayName: proto.String("Insights"),
		Tiles:       []*dashboardsv1.DashboardTileInput{updated},
	}))
	if err != nil {
		t.Fatalf("Upsert #2: %v", err)
	}
	tile := resp2.Msg.GetDashboard().GetTiles()[0]
	if tile.GetUpdateTime().AsTime().Equal(t1) {
		t.Errorf("update_time did not bump after spec change")
	}
	gotKind := tile.GetInsight().GetSpec().GetEvents()[0].GetEvent().GetKind()
	if gotKind != "purchase" {
		t.Errorf("event kind after update = %q, want %q", gotKind, "purchase")
	}
}

// TestHandler_Upsert_DuplicateTileID pins the batch-level guard that rejects
// two tiles sharing a non-empty id in one request. Without this rejection the
// loop would silently run UPDATE twice (last-write-wins) and the response
// would include the same tile object twice — protovalidate can't express the
// cross-element check on a repeated field.
func TestHandler_Upsert_DuplicateTileID(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	s, projectID, svc := newIntegrationServer(t)
	ctx := context.Background()

	dash, err := svc.CreateDashboard(ctx, projectID, "Board", "",
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS, insightsv1.Granularity_GRANULARITY_DAY)
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}
	tileID := upsertSeedTiles(t, s, projectID, dash.ID, seedTile{name: "card", body: "original"})[0]

	_, err = s.Upsert(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceUpsertRequest{
		Id:          proto.String(dash.ID),
		DisplayName: proto.String("Board"),
		Tiles: []*dashboardsv1.DashboardTileInput{
			{
				Id:          proto.String(tileID),
				DisplayName: proto.String("first write"),
				Content: &dashboardsv1.DashboardTileInput_Markdown{
					Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("first")},
				},
			},
			{
				Id:          proto.String(tileID), // duplicate
				DisplayName: proto.String("second write"),
				Content: &dashboardsv1.DashboardTileInput_Markdown{
					Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("second")},
				},
			},
		},
	}))
	assertCode(t, err, connect.CodeInvalidArgument)
	assertReason(t, err, apperr.ReasonInvalidTileContent)

	// Original tile must still have its pre-Upsert body — the rejection ran
	// before any DB write.
	getResp, err := s.Get(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceGetRequest{
		Id: proto.String(dash.ID),
	}))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := getResp.Msg.GetDashboard().GetTiles()[0].GetMarkdown().GetBody(); got != "original" {
		t.Errorf("tile body after rejected upsert = %q, want %q", got, "original")
	}
}

// TestHandler_Upsert_LastWriteWins pins the documented contract (CLAUDE.md
// "Dashboards" — last-write-wins, no optimistic locking) by running two
// concurrent Upserts against the same dashboard and asserting both succeed
// and the final stored state matches *one* of the two inputs verbatim (not
// a merge / interleave). Postgres row locks serialize the transactions
// internally; this test pins the externally-visible contract.
func TestHandler_Upsert_LastWriteWins(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	s, projectID, svc := newIntegrationServer(t)
	ctx := context.Background()

	dash, err := svc.CreateDashboard(ctx, projectID, "Board", "",
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS, insightsv1.Granularity_GRANULARITY_DAY)
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}

	build := func(label string) *dashboardsv1.DashboardsServiceUpsertRequest {
		return &dashboardsv1.DashboardsServiceUpsertRequest{
			Id:          proto.String(dash.ID),
			DisplayName: proto.String("Board " + label),
			Tiles: []*dashboardsv1.DashboardTileInput{
				{
					DisplayName: proto.String("tile-" + label),
					Content: &dashboardsv1.DashboardTileInput_Markdown{
						Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("body-" + label)},
					},
				},
			},
		}
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, 2)
	for i, label := range []string{"A", "B"} {
		wg.Add(1)
		go func(i int, label string) {
			defer wg.Done()
			<-start
			_, errs[i] = s.Upsert(authCtx(projectID), connect.NewRequest(build(label)))
		}(i, label)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Upsert #%d: %v", i, err)
		}
	}

	getResp, err := s.Get(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceGetRequest{
		Id: proto.String(dash.ID),
	}))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	dashGot := getResp.Msg.GetDashboard()
	if len(dashGot.GetTiles()) != 1 {
		t.Fatalf("final tile count = %d, want 1 (each Upsert sends one tile, last-write-wins)", len(dashGot.GetTiles()))
	}
	tile := dashGot.GetTiles()[0]
	// Final state must be one of the two inputs in full — no merging.
	for _, label := range []string{"A", "B"} {
		if dashGot.GetDisplayName() == "Board "+label &&
			tile.GetDisplayName() == "tile-"+label &&
			tile.GetMarkdown().GetBody() == "body-"+label {
			return
		}
	}
	t.Errorf("final state matches neither input verbatim: dashboard=%q, tile=%q, body=%q",
		dashGot.GetDisplayName(), tile.GetDisplayName(), tile.GetMarkdown().GetBody())
}

// TestHandler_Upsert_PositionRoundTripHashStable pins the headline payload_hash
// invariant for positioned tiles: a tile sent with only some GridPosition fields
// (W/H set, X/Y unset), fetched, then re-upserted verbatim must NOT bump
// update_time. The hash function and storage round-trip must agree on the
// presence semantics of the position message — protojson omits the unset X/Y on
// both the store and the read, so the echoed tile hashes identically.
func TestHandler_Upsert_PositionRoundTripHashStable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	s, projectID, svc := newIntegrationServer(t)
	ctx := context.Background()

	dash, err := svc.CreateDashboard(ctx, projectID, "Board", "",
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS, insightsv1.Granularity_GRANULARITY_DAY)
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}

	// Seed a position that sets only W/H (X/Y unset, nil) — a realistic FE shape.
	// The hash + storage round-trip must treat the echoed-back tile as
	// byte-identical so the no-op short-circuit holds.
	initialResp, err := s.Upsert(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceUpsertRequest{
		Id:          proto.String(dash.ID),
		DisplayName: proto.String("Board"),
		Tiles: []*dashboardsv1.DashboardTileInput{
			{
				DisplayName: proto.String("with-position"),
				Content: &dashboardsv1.DashboardTileInput_Markdown{
					Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("body")},
				},
				Position: &dashboardsv1.GridPosition{W: proto.Int32(6), H: proto.Int32(3)},
			},
		},
	}))
	if err != nil {
		t.Fatalf("initial Upsert: %v", err)
	}
	insertedTile := initialResp.Msg.GetDashboard().GetTiles()[0]
	t0 := insertedTile.GetUpdateTime().AsTime()

	// Get the dashboard, copy the returned tile verbatim into a new Upsert.
	// If hash(input) == hash(GetThenUpsert(input)) doesn't hold, the second
	// Upsert will fire an UPDATE and bump update_time.
	getResp, err := s.Get(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceGetRequest{
		Id: proto.String(dash.ID),
	}))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	fetched := getResp.Msg.GetDashboard().GetTiles()[0]

	echoed := &dashboardsv1.DashboardTileInput{
		Id:          proto.String(fetched.GetId()),
		DisplayName: proto.String(fetched.GetDisplayName()),
		Description: proto.String(fetched.GetDescription()),
		Content: &dashboardsv1.DashboardTileInput_Markdown{
			Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String(fetched.GetMarkdown().GetBody())},
		},
		Position: fetched.GetPosition(),
		ViewMode: fetched.ViewMode,
		Compare:  fetched.Compare,
	}
	resp2, err := s.Upsert(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceUpsertRequest{
		Id:          proto.String(dash.ID),
		DisplayName: proto.String("Board"),
		Tiles:       []*dashboardsv1.DashboardTileInput{echoed},
	}))
	if err != nil {
		t.Fatalf("echo Upsert: %v", err)
	}
	t1 := resp2.Msg.GetDashboard().GetTiles()[0].GetUpdateTime().AsTime()
	if !t0.Equal(t1) {
		t.Errorf("tile update_time bumped on Get→Upsert echo of positioned tile: %v -> %v", t0, t1)
	}
}

// TestHandler_Upsert_EmptyDescriptionFullReplaces pins the documented Upsert
// asymmetry vs Update: Upsert is full-replace, so sending an empty description
// clears any previously-stored description. Companion to
// TestHandler_Update_EmptyDescriptionPreservesExisting in handler_test.go,
// which pins the inverse Update semantics.
func TestHandler_Upsert_EmptyDescriptionFullReplaces(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	s, projectID, svc := newIntegrationServer(t)
	ctx := context.Background()

	dash, err := svc.CreateDashboard(ctx, projectID, "Board", "original description",
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS, insightsv1.Granularity_GRANULARITY_DAY)
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}

	if _, err := s.Upsert(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceUpsertRequest{
		Id:          proto.String(dash.ID),
		DisplayName: proto.String("Board"),
		Description: proto.String(""),
	})); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	getResp, err := s.Get(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceGetRequest{
		Id: proto.String(dash.ID),
	}))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := getResp.Msg.GetDashboard().GetDescription(); got != "" {
		t.Errorf("description after empty-Upsert = %q, want empty (full-replace)", got)
	}
}

// TestHandler_Upsert_NoChangeKeepsDashboardUpdateTime pins one of the three
// branches of UpsertDashboardMetadata's gating predicate: when neither tiles
// nor metadata changed, the dashboard's update_time must stay put (the SQL
// WHERE makes the UPDATE a zero-row no-op so moddatetime doesn't fire).
func TestHandler_Upsert_NoChangeKeepsDashboardUpdateTime(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	s, projectID, svc := newIntegrationServer(t)
	ctx := context.Background()

	dash, err := svc.CreateDashboard(ctx, projectID, "Board", "desc",
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS, insightsv1.Granularity_GRANULARITY_DAY)
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}

	// Establish a known baseline via Upsert so the byte-identical re-upsert
	// below truly matches every metadata column (the seed-helper would clear
	// description and leave other fields at zero-defaults).
	baseline := &dashboardsv1.DashboardsServiceUpsertRequest{
		Id:                 proto.String(dash.ID),
		DisplayName:        proto.String("Board"),
		Description:        proto.String("desc"),
		DefaultTimeRange:   commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS.Enum(),
		DefaultGranularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
		Tiles: []*dashboardsv1.DashboardTileInput{
			{
				DisplayName: proto.String("card"),
				Content: &dashboardsv1.DashboardTileInput_Markdown{
					Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("body")},
				},
			},
		},
	}
	seeded, err := s.Upsert(authCtx(projectID), connect.NewRequest(baseline))
	if err != nil {
		t.Fatalf("baseline Upsert: %v", err)
	}
	tileID := seeded.Msg.GetDashboard().GetTiles()[0].GetId()
	t0 := seeded.Msg.GetDashboard().GetUpdateTime().AsTime()

	// Byte-identical re-upsert: same metadata, same tile content (now with
	// the assigned tile id).
	baseline.Tiles[0].Id = proto.String(tileID)
	if _, err := s.Upsert(authCtx(projectID), connect.NewRequest(baseline)); err != nil {
		t.Fatalf("re-Upsert: %v", err)
	}

	after, err := s.Get(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceGetRequest{
		Id: proto.String(dash.ID),
	}))
	if err != nil {
		t.Fatalf("after Get: %v", err)
	}
	t1 := after.Msg.GetDashboard().GetUpdateTime().AsTime()
	if !t0.Equal(t1) {
		t.Errorf("dashboard update_time bumped on byte-identical re-Upsert: %v -> %v", t0, t1)
	}
}

// TestHandler_Upsert_OnlyTileEditBumpsDashboardUpdateTime pins the gating
// predicate's tiles_changed branch: a tile edit with identical dashboard
// metadata must still bump the dashboard's update_time (the row gets re-UPDATEd
// with identical values so the moddatetime trigger fires).
func TestHandler_Upsert_OnlyTileEditBumpsDashboardUpdateTime(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	s, projectID, svc := newIntegrationServer(t)
	ctx := context.Background()

	dash, err := svc.CreateDashboard(ctx, projectID, "Board", "desc",
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS, insightsv1.Granularity_GRANULARITY_DAY)
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}

	baseline := &dashboardsv1.DashboardsServiceUpsertRequest{
		Id:                 proto.String(dash.ID),
		DisplayName:        proto.String("Board"),
		Description:        proto.String("desc"),
		DefaultTimeRange:   commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS.Enum(),
		DefaultGranularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
		Tiles: []*dashboardsv1.DashboardTileInput{
			{
				DisplayName: proto.String("card"),
				Content: &dashboardsv1.DashboardTileInput_Markdown{
					Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("body")},
				},
			},
		},
	}
	seeded, err := s.Upsert(authCtx(projectID), connect.NewRequest(baseline))
	if err != nil {
		t.Fatalf("baseline Upsert: %v", err)
	}
	tileID := seeded.Msg.GetDashboard().GetTiles()[0].GetId()
	t0 := seeded.Msg.GetDashboard().GetUpdateTime().AsTime()

	// Tile body changes; metadata byte-identical.
	baseline.Tiles[0].Id = proto.String(tileID)
	baseline.Tiles[0].Content = &dashboardsv1.DashboardTileInput_Markdown{
		Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("new body")},
	}
	if _, err := s.Upsert(authCtx(projectID), connect.NewRequest(baseline)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	after, err := s.Get(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceGetRequest{
		Id: proto.String(dash.ID),
	}))
	if err != nil {
		t.Fatalf("after Get: %v", err)
	}
	t1 := after.Msg.GetDashboard().GetUpdateTime().AsTime()
	if t1.Equal(t0) || t1.Before(t0) {
		t.Errorf("dashboard update_time did not bump on tile edit: %v -> %v", t0, t1)
	}
}

// TestHandler_Upsert_OnlyMetadataEditBumpsDashboardUpdateTime pins the gating
// predicate's metadata-changed branch: a metadata edit with byte-identical
// tiles must bump the dashboard's update_time and must NOT bump any tile's
// update_time.
func TestHandler_Upsert_OnlyMetadataEditBumpsDashboardUpdateTime(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	s, projectID, svc := newIntegrationServer(t)
	ctx := context.Background()

	dash, err := svc.CreateDashboard(ctx, projectID, "Board", "desc",
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS, insightsv1.Granularity_GRANULARITY_DAY)
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}

	baseline := &dashboardsv1.DashboardsServiceUpsertRequest{
		Id:                 proto.String(dash.ID),
		DisplayName:        proto.String("Board"),
		Description:        proto.String("desc"),
		DefaultTimeRange:   commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS.Enum(),
		DefaultGranularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
		Tiles: []*dashboardsv1.DashboardTileInput{
			{
				DisplayName: proto.String("card"),
				Content: &dashboardsv1.DashboardTileInput_Markdown{
					Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("body")},
				},
			},
		},
	}
	seeded, err := s.Upsert(authCtx(projectID), connect.NewRequest(baseline))
	if err != nil {
		t.Fatalf("baseline Upsert: %v", err)
	}
	tileID := seeded.Msg.GetDashboard().GetTiles()[0].GetId()
	dt0 := seeded.Msg.GetDashboard().GetUpdateTime().AsTime()
	tt0 := seeded.Msg.GetDashboard().GetTiles()[0].GetUpdateTime().AsTime()

	// Display name changes; tile content byte-identical.
	baseline.DisplayName = proto.String("Board renamed")
	baseline.Tiles[0].Id = proto.String(tileID)
	if _, err := s.Upsert(authCtx(projectID), connect.NewRequest(baseline)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	after, err := s.Get(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceGetRequest{
		Id: proto.String(dash.ID),
	}))
	if err != nil {
		t.Fatalf("after Get: %v", err)
	}
	dt1 := after.Msg.GetDashboard().GetUpdateTime().AsTime()
	tt1 := after.Msg.GetDashboard().GetTiles()[0].GetUpdateTime().AsTime()
	if dt1.Equal(dt0) || dt1.Before(dt0) {
		t.Errorf("dashboard update_time did not bump on metadata-only edit: %v -> %v", dt0, dt1)
	}
	if !tt0.Equal(tt1) {
		t.Errorf("tile update_time bumped on metadata-only edit (payload_hash short-circuit failed): %v -> %v", tt0, tt1)
	}
}

// newIntegrationServerTwoProjects sets up Postgres + service + handler with
// two distinct projects in the same org. Returns the handler, both project
// IDs, and the service. Used by cross-project isolation tests.
func newIntegrationServerTwoProjects(t *testing.T) (*Server, string, string, *coredashboards.Service) {
	t.Helper()
	db := testutil.SetupPostgres(t)
	svc := coredashboards.NewService(db.PgRO, db.PgW)

	ctx := context.Background()
	orgID := xid.New().String()
	projectA := xid.New().String()
	projectB := xid.New().String()

	if _, err := db.PgW.Exec(ctx,
		`INSERT INTO orgs (id, display_name) VALUES ($1, $2)`,
		orgID, "test-org"); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	for _, projectID := range []string{projectA, projectB} {
		if _, err := db.PgW.Exec(ctx,
			`INSERT INTO projects (id, org_id, display_name) VALUES ($1, $2, $3)`,
			projectID, orgID, "test-project-"+projectID,
		); err != nil {
			t.Fatalf("insert project: %v", err)
		}
	}

	return &Server{service: svc}, projectA, projectB, svc
}

type seedTile struct {
	name string
	body string
}

// upsertSeedTiles replaces the dashboard's tile set with the given markdown
// tiles in one Upsert call and returns the assigned ids in input order. The
// dashboard's display_name is replaced with "Board"; callers that need a
// different name should follow up with their own Upsert or use
// upsertSeedTilesWithName.
func upsertSeedTiles(t *testing.T, s *Server, projectID, dashboardID string, tiles ...seedTile) []string {
	return upsertSeedTilesWithName(t, s, projectID, dashboardID, "Board", tiles...)
}

// upsertSeedTilesWithName is upsertSeedTiles with an explicit dashboard
// display_name. Use this when a test creates the dashboard with a name other
// than "Board" and the seed Upsert would otherwise silently rename it.
func upsertSeedTilesWithName(t *testing.T, s *Server, projectID, dashboardID, displayName string, tiles ...seedTile) []string {
	t.Helper()
	inputs := make([]*dashboardsv1.DashboardTileInput, len(tiles))
	for i, tile := range tiles {
		inputs[i] = &dashboardsv1.DashboardTileInput{
			DisplayName: proto.String(tile.name),
			Content: &dashboardsv1.DashboardTileInput_Markdown{
				Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String(tile.body)},
			},
		}
	}
	resp, err := s.Upsert(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceUpsertRequest{
		Id:          proto.String(dashboardID),
		DisplayName: proto.String(displayName),
		Tiles:       inputs,
	}))
	if err != nil {
		t.Fatalf("seed Upsert: %v", err)
	}
	ids := make([]string, len(resp.Msg.GetDashboard().GetTiles()))
	for i, tile := range resp.Msg.GetDashboard().GetTiles() {
		ids[i] = tile.GetId()
	}
	return ids
}
