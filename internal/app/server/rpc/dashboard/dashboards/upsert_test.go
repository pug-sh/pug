package dashboards

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/pug-sh/pug/internal/apperr"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
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
	keep := upsertSeedMarkdownTile(t, ctx, s, projectID, dash.ID, "keep", "before")
	toDelete := upsertSeedMarkdownTile(t, ctx, s, projectID, dash.ID, "drop", "doomed")

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
	tileID := upsertSeedMarkdownTile(t, ctx, s, projectID, dash.ID, "card", "body")

	// First Upsert with content matching the seeded tile. After this, the
	// stored payload_hash matches what this exact payload would produce.
	req := &dashboardsv1.DashboardsServiceUpsertRequest{
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
	resp1, err := s.Upsert(authCtx(projectID), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("Upsert #1: %v", err)
	}
	t1 := resp1.Msg.GetDashboard().GetTiles()[0].GetUpdateTime().AsTime()

	// Second Upsert with identical payload should be a no-op at the row level.
	resp2, err := s.Upsert(authCtx(projectID), connect.NewRequest(req))
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
// tiles and no error (spec §5 row "Replace omits all tiles").
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
	upsertSeedMarkdownTile(t, ctx, s, projectID, dash.ID, "first", "x")
	upsertSeedMarkdownTile(t, ctx, s, projectID, dash.ID, "second", "y")

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
// the unchanged tile's update_time is preserved while the changed one's bumps
// (spec §5 row "one tile's content changed").
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
	idStable := upsertSeedMarkdownTile(t, ctx, s, projectID, dash.ID, "stable", "still")
	idChanged := upsertSeedMarkdownTile(t, ctx, s, projectID, dash.ID, "changed", "old body")

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
	if !gotTimes[idChanged].After(initialTimes[idChanged]) {
		t.Errorf("changed tile update_time did not bump: %v -> %v", initialTimes[idChanged], gotTimes[idChanged])
	}
}

// TestHandler_Upsert_DuplicateNameRollsBack verifies that a mid-transaction
// failure (here, a unique-violation on display_name during an INSERT) undoes
// the UPDATE that happened earlier in the same Upsert call (spec §5 row
// "mid-transaction failure"). The earlier-updated tile must end up with its
// pre-Upsert content.
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
	alpha := upsertSeedMarkdownTile(t, ctx, s, projectID, dash.ID, "alpha", "original alpha")
	upsertSeedMarkdownTile(t, ctx, s, projectID, dash.ID, "beta", "original beta")

	// First request slot updates alpha (would succeed in isolation); second
	// slot is a new tile claiming the name "beta" — collides with the seeded
	// row mid-tx. Failure must roll alpha's update back.
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
				// New tile, but display_name collides with the still-present beta.
				DisplayName: proto.String("beta"),
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

// TestHandler_Upsert_KpiUnspecifiedComparePersists pins the spec §5 row
// "compare = COMPARE_PERIOD_UNSPECIFIED on KPI tile → Saved as-is": KPI tiles
// don't require a compare period, and UNSPECIFIED must round-trip rather than
// being rejected or normalized away into something else.
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

// upsertSeedMarkdownTile creates a markdown tile via the existing CreateTile
// RPC (not the Upsert flow) so the seeded row has a stable hash and a known
// id. Returns the assigned tile id.
func upsertSeedMarkdownTile(t *testing.T, ctx context.Context, s *Server, projectID, dashboardID, name, body string) string {
	t.Helper()
	resp, err := s.CreateTile(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceCreateTileRequest{
		DashboardId: proto.String(dashboardID),
		DisplayName: proto.String(name),
		Content: &dashboardsv1.DashboardsServiceCreateTileRequest_Markdown{
			Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String(body)},
		},
	}))
	if err != nil {
		t.Fatalf("seed CreateTile %q: %v", name, err)
	}
	_ = ctx
	return resp.Msg.GetTile().GetId()
}
