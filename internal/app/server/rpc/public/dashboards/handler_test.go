package dashboards

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/rs/xid"
	"google.golang.org/protobuf/proto"

	"github.com/pug-sh/pug/internal/apperr"
	coredashboards "github.com/pug-sh/pug/internal/core/dashboards"
	coreinsights "github.com/pug-sh/pug/internal/core/insights"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	publicdashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/public/dashboards/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/testutil"
)

// TestQuery_ResolvesEnabledShareToken pins the C1 contract end-to-end: the
// public share token is a crypto-random 64-char hex string (NOT a guessable
// 20-char xid), and the unauthenticated handler resolves it to the rendered
// dashboard (tiles included).
func TestQuery_ResolvesEnabledShareToken(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	srv, svc, projectID := newPublicIntegrationServer(t)
	ctx := context.Background()
	_, token := seedSharedMarkdownDashboard(t, ctx, svc, projectID)

	if len(token) != 64 {
		t.Fatalf("share token len = %d, want 64 (hex of 32 crypto-random bytes); got %q", len(token), token)
	}
	if strings.TrimLeft(token, "0123456789abcdef") != "" {
		t.Fatalf("share token %q is not lowercase hex", token)
	}

	resp, err := srv.Query(ctx, connect.NewRequest(&publicdashboardsv1.SharedDashboardsServiceQueryRequest{
		ShareId: proto.String(token),
	}))
	if err != nil {
		t.Fatalf("Query enabled share: %v", err)
	}
	tiles := resp.Msg.GetDashboard().GetTiles()
	if len(tiles) != 1 {
		t.Fatalf("rendered tiles = %d, want 1", len(tiles))
	}
	if got := tiles[0].GetTile().GetMarkdown().GetBody(); got != "# Hello" {
		t.Errorf("markdown body = %q, want %q", got, "# Hello")
	}
}

// TestQuery_UnknownShareToken_NotFound: a well-formed but unknown token 404s,
// and the resource echoed back is only the caller's own input.
func TestQuery_UnknownShareToken_NotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	srv, _, _ := newPublicIntegrationServer(t)
	_, err := srv.Query(context.Background(), connect.NewRequest(&publicdashboardsv1.SharedDashboardsServiceQueryRequest{
		ShareId: proto.String("0000000000000000000000000000000000000000000000000000000000000000"),
	}))
	assertCode(t, err, connect.CodeNotFound)
	assertReason(t, err, apperr.ReasonDashboardNotFound)
}

// TestQuery_DisabledShareToken_NotFound: a token whose share was disabled must
// 404 — the enabled=true predicate is the access-control gate.
func TestQuery_DisabledShareToken_NotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	srv, svc, projectID := newPublicIntegrationServer(t)
	ctx := context.Background()
	dashID, token := seedSharedMarkdownDashboard(t, ctx, svc, projectID)

	// Disable the share; the previously-issued token must stop resolving.
	if _, err := svc.UpdateDashboard(ctx, projectID, dashID, coredashboards.UpdateDashboardInput{
		DisplayName:        "Shared",
		DefaultTimeRange:   commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS,
		DefaultGranularity: insightsv1.Granularity_GRANULARITY_DAY,
		IsPublic:           proto.Bool(false),
	}); err != nil {
		t.Fatalf("disable share: %v", err)
	}

	_, err := srv.Query(ctx, connect.NewRequest(&publicdashboardsv1.SharedDashboardsServiceQueryRequest{
		ShareId: proto.String(token),
	}))
	assertCode(t, err, connect.CodeNotFound)
}

// ----- Helpers -----------------------------------------------------------

func newPublicIntegrationServer(t *testing.T) (*Server, *coredashboards.Service, string) {
	t.Helper()
	db := testutil.SetupPostgres(t)
	ch := testutil.SetupClickHouse(t)
	svc := coredashboards.NewService(db.PgRO, db.PgW)
	// NewExecutor requires a non-nil conn even though the markdown-only dashboards
	// these tests use never invoke it.
	srv := NewServer(svc, coreinsights.NewExecutor(ch.Conn))

	ctx := context.Background()
	orgID := xid.New().String()
	projectID := xid.New().String()
	if _, err := db.PgW.Exec(ctx,
		`INSERT INTO orgs (id, display_name) VALUES ($1, $2)`, orgID, "test-org"); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if _, err := db.PgW.Exec(ctx,
		`INSERT INTO projects (id, org_id, display_name, private_api_key, public_api_key) VALUES ($1, $2, $3, $4, $5)`,
		projectID, orgID, "test-project", xid.New().String()+"priv", xid.New().String()+"pub"); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	return srv, svc, projectID
}

// seedSharedMarkdownDashboard creates a dashboard with one markdown tile, enables
// public sharing, and returns (dashboardID, shareToken).
func seedSharedMarkdownDashboard(t *testing.T, ctx context.Context, svc *coredashboards.Service, projectID string) (string, string) {
	t.Helper()
	dash, err := svc.CreateDashboard(ctx, projectID, "Shared", "",
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS, insightsv1.Granularity_GRANULARITY_DAY)
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}
	if _, err := svc.UpsertDashboard(ctx, projectID, dash.ID, coredashboards.UpsertDashboardInput{
		DisplayName:        "Shared",
		DefaultTimeRange:   commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS,
		DefaultGranularity: insightsv1.Granularity_GRANULARITY_DAY,
		Tiles: []coredashboards.UpsertTileInput{
			{Payload: coredashboards.TilePayload{
				DisplayName: "Notes",
				Content:     coredashboards.MarkdownTile{Body: "# Hello"},
			}},
		},
	}); err != nil {
		t.Fatalf("UpsertDashboard: %v", err)
	}
	dw, err := svc.UpdateDashboard(ctx, projectID, dash.ID, coredashboards.UpdateDashboardInput{
		DisplayName:        "Shared",
		DefaultTimeRange:   commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS,
		DefaultGranularity: insightsv1.Granularity_GRANULARITY_DAY,
		IsPublic:           proto.Bool(true),
	})
	if err != nil {
		t.Fatalf("enable share: %v", err)
	}
	if dw.Share == nil {
		t.Fatal("share unexpectedly nil after enable")
	}
	return dash.ID, dw.Share.ShareToken
}

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
