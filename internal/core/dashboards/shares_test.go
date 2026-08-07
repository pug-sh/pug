package dashboards

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/rs/xid"
	"google.golang.org/protobuf/proto"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

// TestDbwriteShareToDbread_FieldParity fails the build if the read/write
// dashboard_shares structs drift in field count — a forgotten field in
// dbwriteShareToDbread compiles cleanly but silently zeroes a column.
func TestDbwriteShareToDbread_FieldParity(t *testing.T) {
	r := reflect.TypeOf(dbread.DashboardShare{}).NumField()
	w := reflect.TypeOf(dbwrite.DashboardShare{}).NumField()
	if r != w {
		t.Fatalf("dbread.DashboardShare has %d fields, dbwrite has %d — keep dbwriteShareToDbread in sync", r, w)
	}
}

func newShareTestService(t *testing.T) (*Service, *testutil.TestPostgres) {
	t.Helper()
	db := testutil.SetupPostgres(t)
	return NewService(db.PgRO, db.PgW), db
}

func seedProject(t *testing.T, ctx context.Context, db *testutil.TestPostgres) string {
	t.Helper()
	orgID := xid.New().String()
	projectID := xid.New().String()
	if _, err := db.PgW.Exec(ctx, `INSERT INTO orgs (id, display_name) VALUES ($1, $2)`, orgID, "org"); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if _, err := db.PgW.Exec(ctx,
		`INSERT INTO projects (id, org_id, display_name) VALUES ($1, $2, $3)`,
		projectID, orgID, "proj"); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	return projectID
}

func countShares(t *testing.T, ctx context.Context, db *testutil.TestPostgres, dashboardID string) int {
	t.Helper()
	var n int
	if err := db.PgW.QueryRow(ctx, `SELECT count(*) FROM dashboard_shares WHERE dashboard_id = $1`, dashboardID).Scan(&n); err != nil {
		t.Fatalf("count shares: %v", err)
	}
	return n
}

// TestSetShare_ProjectScoped directly pins the project_id predicate in
// UpsertDashboardShare: a caller in another project must not be able to create
// or toggle a share for a dashboard they don't own, and no row may be written.
// This is the tenant-isolation guard the review flagged (C2) — a regression
// dropping the predicate would let one tenant share another's dashboard.
func TestSetShare_ProjectScoped(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	svc, db := newShareTestService(t)
	ctx := context.Background()
	projectA := seedProject(t, ctx, db)
	projectB := seedProject(t, ctx, db)

	dashA, err := svc.CreateDashboard(ctx, projectA, "A", "",
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS, insightsv1.Granularity_GRANULARITY_DAY)
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}

	w := dbwrite.New(db.PgW)

	// Foreign project → not found, and crucially no row created.
	if _, err := setShare(ctx, w, projectB, dashA.ID, true); !errors.Is(err, ErrDashboardNotFound) {
		t.Fatalf("setShare cross-project: err = %v, want ErrDashboardNotFound", err)
	}
	if n := countShares(t, ctx, db, dashA.ID); n != 0 {
		t.Errorf("cross-project setShare created %d rows, want 0", n)
	}

	// Owning project → succeeds with a crypto token.
	share, err := setShare(ctx, w, projectA, dashA.ID, true)
	if err != nil {
		t.Fatalf("setShare owner: %v", err)
	}
	if share == nil || len(share.ShareToken) != 64 {
		t.Fatalf("owner setShare token = %v, want 64-hex", share)
	}
}

// TestSetShare_PreservesTokenAcrossToggle pins the ON CONFLICT contract: the id
// and share_token survive a disable/re-enable cycle (the public link is stable),
// and a disabled share collapses to nil via enabledShare.
func TestSetShare_PreservesTokenAcrossToggle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	svc, db := newShareTestService(t)
	ctx := context.Background()
	projectID := seedProject(t, ctx, db)
	dash, err := svc.CreateDashboard(ctx, projectID, "D", "",
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS, insightsv1.Granularity_GRANULARITY_DAY)
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}
	w := dbwrite.New(db.PgW)

	enabled, err := setShare(ctx, w, projectID, dash.ID, true)
	if err != nil || enabled == nil {
		t.Fatalf("enable: %v share=%v", err, enabled)
	}
	token := enabled.ShareToken

	disabled, err := setShare(ctx, w, projectID, dash.ID, false)
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if disabled != nil {
		t.Errorf("disabled share should collapse to nil, got %v", disabled)
	}

	reenabled, err := setShare(ctx, w, projectID, dash.ID, true)
	if err != nil || reenabled == nil {
		t.Fatalf("re-enable: %v share=%v", err, reenabled)
	}
	if reenabled.ShareToken != token {
		t.Errorf("re-enabled token = %q, want preserved %q", reenabled.ShareToken, token)
	}
}

// TestLookupShare_ThreeWay pins lookupShare's contract: enabled → the share,
// disabled → nil, no row → nil.
func TestLookupShare_ThreeWay(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	svc, db := newShareTestService(t)
	ctx := context.Background()
	projectID := seedProject(t, ctx, db)
	dash, err := svc.CreateDashboard(ctx, projectID, "D", "",
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS, insightsv1.Granularity_GRANULARITY_DAY)
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}
	r := dbread.New(db.PgRO)
	w := dbwrite.New(db.PgW)

	// No row yet.
	if got, err := lookupShare(ctx, r, dash.ID); err != nil || got != nil {
		t.Fatalf("no-row lookupShare = (%v, %v), want (nil, nil)", got, err)
	}
	// Enabled.
	if _, err := setShare(ctx, w, projectID, dash.ID, true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if got, err := lookupShare(ctx, r, dash.ID); err != nil || got == nil {
		t.Fatalf("enabled lookupShare = (%v, %v), want non-nil", got, err)
	}
	// Disabled.
	if _, err := setShare(ctx, w, projectID, dash.ID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if got, err := lookupShare(ctx, r, dash.ID); err != nil || got != nil {
		t.Fatalf("disabled lookupShare = (%v, %v), want (nil, nil)", got, err)
	}
}

// TestUpdateDashboard_ListReflectsShare pins S1: ListDashboards populates
// share_id (via Share) consistently with Get, only for enabled shares.
func TestUpdateDashboard_ListReflectsShare(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	svc, db := newShareTestService(t)
	ctx := context.Background()
	projectID := seedProject(t, ctx, db)
	shared, err := svc.CreateDashboard(ctx, projectID, "Shared", "",
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS, insightsv1.Granularity_GRANULARITY_DAY)
	if err != nil {
		t.Fatalf("CreateDashboard shared: %v", err)
	}
	private, err := svc.CreateDashboard(ctx, projectID, "Private", "",
		commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS, insightsv1.Granularity_GRANULARITY_DAY)
	if err != nil {
		t.Fatalf("CreateDashboard private: %v", err)
	}
	if _, err := svc.UpdateDashboard(ctx, projectID, shared.ID, UpdateDashboardInput{
		DisplayName:        "Shared",
		DefaultTimeRange:   commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS,
		DefaultGranularity: insightsv1.Granularity_GRANULARITY_DAY,
		IsPublic:           proto.Bool(true),
	}); err != nil {
		t.Fatalf("enable share: %v", err)
	}

	list, err := svc.ListDashboards(ctx, projectID)
	if err != nil {
		t.Fatalf("ListDashboards: %v", err)
	}
	got := map[string]*dbread.DashboardShare{}
	for _, dw := range list {
		got[dw.Dashboard.ID] = dw.Share
	}
	if got[shared.ID] == nil || got[shared.ID].ShareToken == "" {
		t.Errorf("shared dashboard missing Share in List: %v", got[shared.ID])
	}
	if got[private.ID] != nil {
		t.Errorf("private dashboard should have nil Share in List, got %v", got[private.ID])
	}
}
