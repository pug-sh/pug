package dashboards

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/rs/xid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/apperr"
	coreinsights "github.com/pug-sh/pug/internal/core/insights"
	coreprojects "github.com/pug-sh/pug/internal/core/projects"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/testutil"
)

// authCtx returns a context with a Principal that has a Project — what
// MustGetPrincipalWithProject expects in the happy path.
func authCtx(projectID string) context.Context {
	return authn.SetInfo(context.Background(), &rpc.Principal{
		Project: &dbread.Project{ID: projectID},
	})
}

// ----- Unauthenticated paths (no DB needed) ----------------------------
//
// Every handler entry must return CodeUnauthenticated when the context has
// no principal. Pins the auth-failure → CodeUnauthenticated mapping; a
// regression that drops the MustGetPrincipalWithProject call would silently
// fall through and panic on a nil service deref.

func TestHandler_Create_Unauthenticated(t *testing.T) {
	s := &Server{}
	_, err := s.Create(context.Background(), connect.NewRequest(&dashboardsv1.DashboardsServiceCreateRequest{
		DisplayName: proto.String("x"),
	}))
	assertCode(t, err, connect.CodeUnauthenticated)
}

func TestHandler_List_Unauthenticated(t *testing.T) {
	s := &Server{}
	_, err := s.List(context.Background(), connect.NewRequest(&dashboardsv1.DashboardsServiceListRequest{}))
	assertCode(t, err, connect.CodeUnauthenticated)
}

func TestHandler_Get_Unauthenticated(t *testing.T) {
	s := &Server{}
	_, err := s.Get(context.Background(), connect.NewRequest(&dashboardsv1.DashboardsServiceGetRequest{
		Id: proto.String("x"),
	}))
	assertCode(t, err, connect.CodeUnauthenticated)
}

func TestHandler_UpdateDisplayName_Unauthenticated(t *testing.T) {
	s := &Server{}
	_, err := s.UpdateDisplayName(context.Background(), connect.NewRequest(&dashboardsv1.DashboardsServiceUpdateDisplayNameRequest{
		Id:          proto.String("x"),
		DisplayName: proto.String("y"),
	}))
	assertCode(t, err, connect.CodeUnauthenticated)
}

func TestHandler_Delete_Unauthenticated(t *testing.T) {
	s := &Server{}
	_, err := s.Delete(context.Background(), connect.NewRequest(&dashboardsv1.DashboardsServiceDeleteRequest{
		Id: proto.String("x"),
	}))
	assertCode(t, err, connect.CodeUnauthenticated)
}

func TestHandler_CreateTile_Unauthenticated(t *testing.T) {
	s := &Server{}
	_, err := s.CreateTile(context.Background(), connect.NewRequest(&dashboardsv1.DashboardsServiceCreateTileRequest{
		DashboardId: proto.String("x"),
		Content: &dashboardsv1.DashboardsServiceCreateTileRequest_Markdown{
			Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("y")},
		},
	}))
	assertCode(t, err, connect.CodeUnauthenticated)
}

func TestHandler_UpdateTile_Unauthenticated(t *testing.T) {
	s := &Server{}
	_, err := s.UpdateTile(context.Background(), connect.NewRequest(&dashboardsv1.DashboardsServiceUpdateTileRequest{
		Id:          proto.String("x"),
		DashboardId: proto.String("d"),
		Content: &dashboardsv1.DashboardsServiceUpdateTileRequest_Markdown{
			Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("y")},
		},
	}))
	assertCode(t, err, connect.CodeUnauthenticated)
}

func TestHandler_DeleteTile_Unauthenticated(t *testing.T) {
	s := &Server{}
	_, err := s.DeleteTile(context.Background(), connect.NewRequest(&dashboardsv1.DashboardsServiceDeleteTileRequest{
		Id:          proto.String("x"),
		DashboardId: proto.String("d"),
	}))
	assertCode(t, err, connect.CodeUnauthenticated)
}

func TestHandler_QueryDashboard_Unauthenticated(t *testing.T) {
	s := &Server{}
	_, err := s.QueryDashboard(context.Background(), connect.NewRequest(&dashboardsv1.DashboardsServiceQueryDashboardRequest{
		DashboardId: proto.String("x"),
	}))
	assertCode(t, err, connect.CodeUnauthenticated)
}

// ----- Service-error → connect.Code mapping (integration) ----------------
//
// The handler translates sentinels (ErrDashboardNotFound, ErrDashboardTileNotFound,
// ErrDashboardTileDisplayNameConflict) into specific connect codes. A regression
// that adds a new sentinel without wiring it up would silently fall through to
// CodeInternal — a user-facing wrong-HTTP-status bug. These tests pin the
// mappings end-to-end against a real Postgres.

func TestHandler_Get_NotFound_MapsToCodeNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	s, projectID, _ := newIntegrationServer(t)

	_, err := s.Get(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceGetRequest{
		Id: proto.String("nonexistent_dashboard"),
	}))
	assertCode(t, err, connect.CodeNotFound)
	assertReason(t, err, apperr.ReasonDashboardNotFound)
}

func TestHandler_QueryDashboard_NotFound_MapsToCodeNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	s, projectID, _ := newIntegrationServer(t)

	_, err := s.QueryDashboard(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceQueryDashboardRequest{
		DashboardId: proto.String("nonexistent_dashboard"),
	}))
	assertCode(t, err, connect.CodeNotFound)
	assertReason(t, err, apperr.ReasonDashboardNotFound)
}

func TestHandler_QueryDashboard_ReturnsTrendResults(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	db := testutil.SetupPostgres(t)
	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()

	svc := coreprojects.NewService(db.PgRO, db.PgW, nil)
	executor := coreinsights.NewExecutor(ch.Conn)
	s := NewServer(svc, executor)

	orgID := xid.New().String()
	projectID := xid.New().String()
	if _, err := db.PgW.Exec(ctx,
		`INSERT INTO orgs (id, display_name) VALUES ($1, $2)`,
		orgID, "test-org"); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if _, err := db.PgW.Exec(ctx,
		`INSERT INTO projects (id, org_id, display_name, private_api_key, public_api_key) VALUES ($1, $2, $3, $4, $5)`,
		projectID, orgID, "test-project",
		xid.New().String()+"priv",
		xid.New().String()+"pub",
	); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	seedDashboardQueryEvents(t, ctx, ch, projectID)

	dashboard, err := svc.CreateDashboard(ctx, projectID, "Overview", "")
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}

	queryFrom := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	queryTo := time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC)
	insightQuery := &insightsv1.QueryRequest{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
		TimeRange: &commonv1.TimeRange{
			From: timestamppb.New(queryFrom),
			To:   timestamppb.New(queryTo),
		},
		Events: []*insightsv1.EventQuery{
			{
				Event:       &commonv1.EventFilter{Kind: proto.String("page_view")},
				Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum(),
			},
		},
	}
	if _, err := svc.CreateDashboardTile(ctx, projectID, dashboard.ID, "Page views", "",
		coreprojects.InsightTile{Query: insightQuery},
		dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE,
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS,
		nil,
	); err != nil {
		t.Fatalf("CreateDashboardTile: %v", err)
	}

	resp, err := s.QueryDashboard(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceQueryDashboardRequest{
		DashboardId: proto.String(dashboard.ID),
	}))
	if err != nil {
		t.Fatalf("QueryDashboard: %v", err)
	}
	if len(resp.Msg.GetResults()) != 1 {
		t.Fatalf("results = %d, want 1", len(resp.Msg.GetResults()))
	}

	result := resp.Msg.GetResults()[0]
	if result.GetErrorMessage() != "" {
		t.Fatalf("unexpected tile error: %s", result.GetErrorMessage())
	}
	series := result.GetResult().GetTrends().GetSeries()
	if len(series) != 1 {
		t.Fatalf("series = %d, want 1", len(series))
	}
	if got := len(series[0].GetPoints()); got != 3 {
		t.Fatalf("points = %d, want 3 daily buckets", got)
	}
}

func TestHandler_Delete_NotFound_MapsToCodeNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	s, projectID, _ := newIntegrationServer(t)

	_, err := s.Delete(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceDeleteRequest{
		Id: proto.String("nonexistent_dashboard"),
	}))
	assertCode(t, err, connect.CodeNotFound)
}

func TestHandler_UpdateDisplayName_NotFound_MapsToCodeNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	s, projectID, _ := newIntegrationServer(t)

	_, err := s.UpdateDisplayName(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceUpdateDisplayNameRequest{
		Id:          proto.String("nonexistent_dashboard"),
		DisplayName: proto.String("renamed"),
	}))
	assertCode(t, err, connect.CodeNotFound)
}

func TestHandler_CreateTile_DashboardNotFound_MapsToCodeNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	s, projectID, _ := newIntegrationServer(t)

	_, err := s.CreateTile(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceCreateTileRequest{
		DashboardId: proto.String("nonexistent_dashboard"),
		Content: &dashboardsv1.DashboardsServiceCreateTileRequest_Markdown{
			Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("body")},
		},
	}))
	assertCode(t, err, connect.CodeNotFound)
	assertReason(t, err, apperr.ReasonDashboardNotFound)
}

func TestHandler_CreateTile_DisplayNameConflict_MapsToCodeAlreadyExists(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	s, projectID, svc := newIntegrationServer(t)

	dashboard, err := svc.CreateDashboard(context.Background(), projectID, "Conflict Dashboard", "")
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}
	t.Cleanup(func() { _ = svc.DeleteDashboard(context.Background(), projectID, dashboard.ID) })

	if _, err := svc.CreateDashboardTile(context.Background(), projectID, dashboard.ID, "Same Name", "",
		coreprojects.MarkdownTile{Body: "first"},
		dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED,
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_UNSPECIFIED,
		nil); err != nil {
		t.Fatalf("first tile: %v", err)
	}

	_, err = s.CreateTile(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceCreateTileRequest{
		DashboardId: proto.String(dashboard.ID),
		DisplayName: proto.String("Same Name"),
		Content: &dashboardsv1.DashboardsServiceCreateTileRequest_Markdown{
			Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("second")},
		},
	}))
	assertCode(t, err, connect.CodeAlreadyExists)
	assertReason(t, err, apperr.ReasonDashboardTileNameConflict)
}

func TestHandler_UpdateTile_NotFound_MapsToCodeNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	s, projectID, _ := newIntegrationServer(t)

	_, err := s.UpdateTile(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceUpdateTileRequest{
		Id:          proto.String("nonexistent_tile"),
		DashboardId: proto.String("nonexistent_dashboard"),
		Content: &dashboardsv1.DashboardsServiceUpdateTileRequest_Markdown{
			Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("body")},
		},
	}))
	assertCode(t, err, connect.CodeNotFound)
	assertReason(t, err, apperr.ReasonDashboardTileNotFound)
}

func TestHandler_DeleteTile_NotFound_MapsToCodeNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	s, projectID, _ := newIntegrationServer(t)

	_, err := s.DeleteTile(authCtx(projectID), connect.NewRequest(&dashboardsv1.DashboardsServiceDeleteTileRequest{
		Id:          proto.String("nonexistent_tile"),
		DashboardId: proto.String("nonexistent_dashboard"),
	}))
	assertCode(t, err, connect.CodeNotFound)
}

// ----- Helpers -----------------------------------------------------------

func assertCode(t *testing.T, err error, want connect.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var ae *apperr.Error
	if errors.As(err, &ae) {
		if ae.Code() != want {
			t.Errorf("got code %v, want %v (err: %v)", ae.Code(), want, err)
		}
		return
	}
	if got := connect.CodeOf(err); got != want {
		t.Errorf("got code %v, want %v (err: %v)", got, want, err)
	}
}

func assertReason(t *testing.T, err error, want apperr.Reason) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var ae *apperr.Error
	if !errors.As(err, &ae) {
		t.Errorf("expected *apperr.Error to assert reason %q, got %T", want, err)
		return
	}
	if ae.Reason() != want {
		t.Errorf("got reason %q, want %q (err: %v)", ae.Reason(), want, err)
	}
}

// newIntegrationServer sets up a real Postgres + service + handler. Returns
// the handler, a project ID with a backing org row, and the service (for
// callers that need to seed dashboards/tiles).
func newIntegrationServer(t *testing.T) (*Server, string, *coreprojects.Service) {
	t.Helper()
	db := testutil.SetupPostgres(t)
	svc := coreprojects.NewService(db.PgRO, db.PgW, nil)

	ctx := context.Background()
	orgID := xid.New().String()
	projectID := xid.New().String()

	if _, err := db.PgW.Exec(ctx,
		`INSERT INTO orgs (id, display_name) VALUES ($1, $2)`,
		orgID, "test-org"); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if _, err := db.PgW.Exec(ctx,
		`INSERT INTO projects (id, org_id, display_name, private_api_key, public_api_key) VALUES ($1, $2, $3, $4, $5)`,
		projectID, orgID, "test-project",
		xid.New().String()+"priv",
		xid.New().String()+"pub",
	); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	return &Server{service: svc}, projectID, svc
}

func seedDashboardQueryEvents(t *testing.T, ctx context.Context, ch *testutil.TestClickHouse, projectID string) {
	t.Helper()

	type event struct {
		day  int
		user string
	}

	events := []event{
		{1, "alice"},
		{1, "bob"},
		{1, "charlie"},
		{2, "alice"},
		{2, "bob"},
		{3, "alice"},
	}

	for _, e := range events {
		occurTime := time.Date(2024, 1, e.day, 12, 0, 0, 0, time.UTC)
		batch, err := ch.Conn.PrepareBatch(ctx,
			"INSERT INTO events (project_id, event_id, kind, distinct_id, occur_time, auto_properties)")
		if err != nil {
			t.Fatalf("PrepareBatch: %v", err)
		}
		if err := batch.Append(projectID, uuid.New().String(), "page_view", e.user, occurTime, map[string]any{}); err != nil {
			t.Fatalf("Append: %v", err)
		}
		if err := batch.Send(); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
}
