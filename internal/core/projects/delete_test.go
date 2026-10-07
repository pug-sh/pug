package projects_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rs/xid"

	coredashboards "github.com/pug-sh/pug/internal/core/dashboards"
	"github.com/pug-sh/pug/internal/core/projects"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

func TestDeleteProjectHidesAndRevokes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	rd := testutil.SetupRedis(t)
	ctx := context.Background()
	write := dbwrite.New(db.PgW)

	orgID, customerID := xid.New().String(), xid.New().String()
	if _, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: orgID, DisplayName: "Delete Org"}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: customerID, Email: customerID + "@test.com", DisplayName: "D", PasswordHash: "h",
	}); err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID: orgID, CustomerID: customerID, Role: "ORG_ROLE_ADMIN",
	}); err != nil {
		t.Fatalf("CreateOrgMember: %v", err)
	}

	repo := projects.NewRepo(dbread.New(db.PgRO), rd.Client)
	svc := projects.NewService(db.PgRO, db.PgW, repo)
	dashboards := coredashboards.NewService(db.PgRO, db.PgW)

	proj, err := svc.CreateProject(ctx, orgID, "Doomed", "")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	keys, err := svc.ListApiKeys(ctx, proj.ID)
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListApiKeys = %d keys, %v; want the starter key", len(keys), err)
	}
	pubKey := keys[0].Token
	prv, err := svc.CreateApiKey(ctx, proj.ID, projects.KindPrivate, "")
	if err != nil {
		t.Fatalf("CreateApiKey: %v", err)
	}
	if _, err := svc.UpdateFCMServiceJSON(ctx, dbwrite.UpdateFCMServiceJSONParams{
		ID: proj.ID, OrgID: orgID, FcmServiceJson: postgres.NewText(`{"type":"service_account"}`),
	}); err != nil {
		t.Fatalf("UpdateFCMServiceJSON: %v", err)
	}
	dash, err := write.CreateDashboard(ctx, dbwrite.CreateDashboardParams{
		ID: "dash-delete000000000", ProjectID: proj.ID, DisplayName: "Board",
		DefaultTimeRange: "TIME_RANGE_PRESET_LAST_30_DAYS", DefaultGranularity: "GRANULARITY_DAY",
	})
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}
	share := func(t *testing.T, id, token string) {
		t.Helper()
		if _, err := write.UpsertDashboardShare(ctx, dbwrite.UpsertDashboardShareParams{
			ID: id, ShareToken: token, Enabled: true, DashboardID: dash.ID, ProjectID: proj.ID,
		}); err != nil {
			t.Fatalf("UpsertDashboardShare: %v", err)
		}
	}
	shareToken := strings.Repeat("a", 64)
	share(t, "share-before00000000", shareToken)
	if _, err := dashboards.GetSharedDashboard(ctx, shareToken); err != nil {
		t.Fatalf("share link before the delete: %v", err)
	}
	if _, err := write.CreateCampaign(ctx, dbwrite.CreateCampaignParams{
		ID: "camp-delete000000000", Name: "Push", NotificationData: map[string]any{},
		ProjectID: proj.ID, ScheduledTime: postgres.NewTimestamptz(time.Now()), Status: "scheduled",
	}); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	// Cached, so a delete that left the cache alone would keep both keys working.
	if _, err := repo.GetProjectByPrivateApiKey(ctx, prv.RawKey); err != nil {
		t.Fatalf("resolve private key: %v", err)
	}
	if _, err := repo.GetProjectByPublicApiKey(ctx, pubKey); err != nil {
		t.Fatalf("resolve public key: %v", err)
	}

	// A sibling in the same org, which the delete must leave alone.
	sibling, err := svc.CreateProject(ctx, orgID, "Sibling", "")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	siblingKeys, err := svc.ListApiKeys(ctx, sibling.ID)
	if err != nil || len(siblingKeys) != 1 {
		t.Fatalf("ListApiKeys = %d keys, %v; want the starter key", len(siblingKeys), err)
	}
	siblingDash, err := write.CreateDashboard(ctx, dbwrite.CreateDashboardParams{
		ID: "dash-sibling00000000", ProjectID: sibling.ID, DisplayName: "Board",
		DefaultTimeRange: "TIME_RANGE_PRESET_LAST_30_DAYS", DefaultGranularity: "GRANULARITY_DAY",
	})
	if err != nil {
		t.Fatalf("CreateDashboard: %v", err)
	}
	siblingShare := strings.Repeat("c", 64)
	if _, err := write.UpsertDashboardShare(ctx, dbwrite.UpsertDashboardShareParams{
		ID: "share-sibling0000000", ShareToken: siblingShare, Enabled: true, DashboardID: siblingDash.ID, ProjectID: sibling.ID,
	}); err != nil {
		t.Fatalf("UpsertDashboardShare: %v", err)
	}
	if _, err := write.CreateCampaign(ctx, dbwrite.CreateCampaignParams{
		ID: "camp-sibling00000000", Name: "Push", NotificationData: map[string]any{},
		ProjectID: sibling.ID, ScheduledTime: postgres.NewTimestamptz(time.Now()), Status: "scheduled",
	}); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}

	if err := svc.DeleteProject(ctx, orgID, proj.ID, "customer "+customerID); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}

	t.Run("the deletion is queued", func(t *testing.T) {
		var status, name, org, requestedBy string
		if err := db.PgRO.QueryRow(ctx,
			"select status, display_name, coalesce(org_id, ''), requested_by from project_deletions where project_id = $1",
			proj.ID).Scan(&status, &name, &org, &requestedBy); err != nil {
			t.Fatalf("read project_deletions: %v", err)
		}
		if status != "pending" || name != "Doomed" || org != orgID || requestedBy != "customer "+customerID {
			t.Errorf("deletion = (%q, %q, %q, %q), want (pending, Doomed, %s, customer %s)",
				status, name, org, requestedBy, orgID, customerID)
		}
	})

	t.Run("every project query skips it", func(t *testing.T) {
		if _, err := svc.GetProjectByID(ctx, proj.ID); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("GetProjectByID err = %v, want pgx.ErrNoRows", err)
		}
		list, err := svc.GetProjectsByOrgID(ctx, orgID)
		if err != nil {
			t.Fatalf("GetProjectsByOrgID: %v", err)
		}
		if slices.ContainsFunc(list, func(p dbread.Project) bool { return p.ID == proj.ID }) {
			t.Error("the org's project list still has it")
		}
		// Same query as the x-project-id lookup.
		if ok, err := svc.ProjectExistsForOrgMember(ctx, proj.ID, customerID); err != nil || ok {
			t.Errorf("ProjectExistsForOrgMember = %v, %v; want false", ok, err)
		}
	})

	t.Run("its keys stop resolving", func(t *testing.T) {
		if _, err := repo.GetProjectByPrivateApiKey(ctx, prv.RawKey); err == nil {
			t.Error("the private key still resolves")
		}
		if _, err := repo.GetProjectByPublicApiKey(ctx, pubKey); err == nil {
			t.Error("the public key still resolves")
		}
		if left, err := svc.ListApiKeys(ctx, proj.ID); err != nil || len(left) != 0 {
			t.Errorf("ListApiKeys = %d keys, %v; want none", len(left), err)
		}
		// Keys created after the request, which the request could not delete.
		latePrv, err := svc.CreateApiKey(ctx, proj.ID, projects.KindPrivate, "")
		if err != nil {
			t.Fatalf("CreateApiKey: %v", err)
		}
		if _, err := repo.GetProjectByPrivateApiKey(ctx, latePrv.RawKey); err == nil {
			t.Error("a private key created after the delete resolves")
		}
		latePub, err := svc.CreateApiKey(ctx, proj.ID, projects.KindPublic, "")
		if err != nil {
			t.Fatalf("CreateApiKey: %v", err)
		}
		if _, err := repo.GetProjectByPublicApiKey(ctx, latePub.RawKey); err == nil {
			t.Error("a public key created after the delete resolves")
		}
	})

	t.Run("its share links 404", func(t *testing.T) {
		if _, err := dashboards.GetSharedDashboard(ctx, shareToken); !errors.Is(err, coredashboards.ErrDashboardNotFound) {
			t.Errorf("share link err = %v, want ErrDashboardNotFound", err)
		}
		// A share created after the request, which the request could not delete.
		lateToken := strings.Repeat("b", 64)
		share(t, "share-after000000000", lateToken)
		if _, err := dashboards.GetSharedDashboard(ctx, lateToken); !errors.Is(err, coredashboards.ErrDashboardNotFound) {
			t.Errorf("late share link err = %v, want ErrDashboardNotFound", err)
		}
	})

	t.Run("the row stays, hidden, without its FCM key", func(t *testing.T) {
		var hidden, fcmCleared bool
		if err := db.PgRO.QueryRow(ctx,
			"select deletion_time is not null, fcm_service_json is null from projects where id = $1",
			proj.ID).Scan(&hidden, &fcmCleared); err != nil {
			t.Fatalf("read projects row: %v", err)
		}
		if !hidden || !fcmCleared {
			t.Errorf("hidden = %t, FCM key cleared = %t; want both", hidden, fcmCleared)
		}
	})

	t.Run("its campaigns are deleted", func(t *testing.T) {
		var n int
		if err := db.PgRO.QueryRow(ctx, "select count(*) from campaigns where project_id = $1", proj.ID).Scan(&n); err != nil {
			t.Fatalf("count campaigns: %v", err)
		}
		if n != 0 {
			t.Errorf("%d campaigns left, want 0", n)
		}
	})

	t.Run("it cannot be changed or deleted again", func(t *testing.T) {
		if _, err := svc.UpdateProjectMeta(ctx, dbwrite.UpdateProjectMetaParams{
			ID: proj.ID, OrgID: orgID, DisplayName: postgres.NewText("Revived"),
		}); !errors.Is(err, projects.ErrProjectNotFound) {
			t.Errorf("UpdateProjectMeta err = %v, want ErrProjectNotFound", err)
		}
		if _, err := svc.UpdateFCMServiceJSON(ctx, dbwrite.UpdateFCMServiceJSONParams{
			ID: proj.ID, OrgID: orgID, FcmServiceJson: postgres.NewText(`{}`),
		}); !errors.Is(err, projects.ErrProjectNotFound) {
			t.Errorf("UpdateFCMServiceJSON err = %v, want ErrProjectNotFound", err)
		}
		if err := svc.DeleteProject(ctx, orgID, proj.ID, "customer "+customerID); !errors.Is(err, projects.ErrProjectNotFound) {
			t.Errorf("second DeleteProject err = %v, want ErrProjectNotFound", err)
		}
	})

	t.Run("its name is free", func(t *testing.T) {
		if _, err := svc.CreateProject(ctx, orgID, "Doomed", ""); err != nil {
			t.Errorf("CreateProject with the deleted project's name: %v", err)
		}
	})

	t.Run("its sibling is untouched", func(t *testing.T) {
		list, err := svc.GetProjectsByOrgID(ctx, orgID)
		if err != nil {
			t.Fatalf("GetProjectsByOrgID: %v", err)
		}
		if !slices.ContainsFunc(list, func(p dbread.Project) bool { return p.ID == sibling.ID }) {
			t.Error("the sibling left the org's project list")
		}
		if _, err := repo.GetProjectByPublicApiKey(ctx, siblingKeys[0].Token); err != nil {
			t.Errorf("the sibling's key stopped resolving: %v", err)
		}
		if _, err := dashboards.GetSharedDashboard(ctx, siblingShare); err != nil {
			t.Errorf("the sibling's share link stopped serving: %v", err)
		}
		var n int
		if err := db.PgRO.QueryRow(ctx, "select count(*) from campaigns where project_id = $1", sibling.ID).Scan(&n); err != nil {
			t.Fatalf("count campaigns: %v", err)
		}
		if n != 1 {
			t.Errorf("the sibling has %d campaigns, want 1", n)
		}
	})
}

// A delete that waited on another's lock finds the project already hidden. It
// must report not found rather than trip over the first one's ledger row.
func TestDeleteProjectConcurrentDeleteIsNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	ctx := t.Context()
	svc := projects.NewService(db.PgRO, db.PgW, nil)

	orgID := xid.New().String()
	if _, err := dbwrite.New(db.PgW).CreateOrg(ctx, dbwrite.CreateOrgParams{ID: orgID, DisplayName: "Race Org"}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	proj, err := svc.CreateProject(ctx, orgID, "Raced", "")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	// The first delete, held open on its lock.
	tx, err := db.PgW.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	first := dbwrite.New(tx)
	if _, err := first.GetProjectByIDForUpdate(ctx, dbwrite.GetProjectByIDForUpdateParams{OrgID: orgID, ID: proj.ID}); err != nil {
		t.Fatalf("lock: %v", err)
	}

	second := make(chan error, 1)
	go func() { second <- svc.DeleteProject(ctx, orgID, proj.ID, "customer second") }()
	waitForLockWait(t, db)

	if err := first.CreateProjectDeletion(ctx, dbwrite.CreateProjectDeletionParams{
		DisplayName: "Raced", OrgID: postgres.NewText(orgID), ProjectID: proj.ID, RequestedBy: "customer first",
	}); err != nil {
		t.Fatalf("CreateProjectDeletion: %v", err)
	}
	if err := first.HideProject(ctx, proj.ID); err != nil {
		t.Fatalf("HideProject: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	select {
	case err := <-second:
		if !errors.Is(err, projects.ErrProjectNotFound) {
			t.Errorf("concurrent DeleteProject err = %v, want ErrProjectNotFound", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second delete never returned")
	}
}

// waitForLockWait returns once a session in the test database is waiting on a lock.
func waitForLockWait(t *testing.T, db *testutil.TestPostgres) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := db.PgRO.QueryRow(t.Context(),
			"select count(*) from pg_stat_activity where datname = current_database() and wait_event_type = 'Lock'",
		).Scan(&waiting); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no session ever waited on the lock")
}
