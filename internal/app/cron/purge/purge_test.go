package purge

import (
	"errors"
	"testing"
	"time"

	"github.com/rs/xid"

	"github.com/pug-sh/pug/internal/app/cron"
	coreprojects "github.com/pug-sh/pug/internal/core/projects"
	corepurge "github.com/pug-sh/pug/internal/core/purge"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

func TestMain(m *testing.M) { testutil.Main(m) }

// A deletion pending a day sets off the server's startup check, and one pass
// clears it.
func TestRunErasesADueDeletion(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	ch := testutil.SetupClickHouse(t)
	t.Setenv("DATABASE_URL", pg.PgW.Config().ConnString())
	t.Setenv("CLICKHOUSE_URL", ch.URL)
	projectID := seedDeletion(t, pg, time.Now().Add(-48*time.Hour))

	if !stalled(t, pg) {
		t.Fatal("a deletion pending for two days is not reported as stalled")
	}
	if err := Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := status(t, pg, projectID); got != "done" {
		t.Errorf("status %q, want done", got)
	}
	if stalled(t, pg) {
		t.Error("still reported as stalled after the pass")
	}
}

// A job that stops after a deletion's first round leaves it deleting, not
// pending.
func TestStalledCoversADeletionLeftDeleting(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	projectID := seedDeletion(t, pg, time.Now().Add(-48*time.Hour))
	for _, tc := range []struct {
		roundAge string
		want     bool
	}{
		{roundAge: "1 hour", want: false},
		{roundAge: "2 days", want: true},
	} {
		if _, err := pg.PgW.Exec(t.Context(),
			"update project_deletions set status = 'deleting', round_started_at = now() - $1::interval where project_id = $2",
			tc.roundAge, projectID); err != nil {
			t.Fatalf("start a round: %v", err)
		}
		if got := stalled(t, pg); got != tc.want {
			t.Errorf("last round %s ago: stalled = %t, want %t", tc.roundAge, got, tc.want)
		}
	}
}

// Contention is not failure, and the pass must not run beside the holder.
func TestRunExitsZeroWhileAnotherPassHoldsTheLock(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	ch := testutil.SetupClickHouse(t)
	t.Setenv("DATABASE_URL", pg.PgW.Config().ConnString())
	t.Setenv("CLICKHOUSE_URL", ch.URL)
	projectID := seedDeletion(t, pg, time.Now().Add(-2*time.Hour))

	ctx := t.Context()
	holder, err := pg.PgW.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder tx: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if acquired, err := dbwrite.New(holder).TryCronLock(ctx, int64(cron.LockPurge)); err != nil || !acquired {
		t.Fatalf("TryCronLock = %t, %v", acquired, err)
	}

	if err := Run(ctx); err != nil {
		t.Fatalf("Run = %v, want nil while another pass holds the lock", err)
	}
	if got := status(t, pg, projectID); got != "pending" {
		t.Errorf("status %q, want pending: the pass ran beside the lock holder", got)
	}
}

// A failed pass is the CronJob's only failure signal.
func TestRunFailsWhenThePassFails(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	ch := testutil.SetupClickHouse(t)
	t.Setenv("DATABASE_URL", pg.PgW.Config().ConnString())
	t.Setenv("CLICKHOUSE_URL", ch.URL)
	projectID := seedDeletion(t, pg, time.Now().Add(-2*time.Hour))
	// The pass refuses to erase a live project.
	if _, err := pg.PgW.Exec(t.Context(),
		"update projects set deletion_time = null where id = $1", projectID); err != nil {
		t.Fatalf("unhide the project: %v", err)
	}

	if err := Run(t.Context()); !errors.Is(err, corepurge.ErrLiveProject) {
		t.Fatalf("Run = %v, want ErrLiveProject", err)
	}
}

func TestRunFailsWithoutClickHouse(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	t.Setenv("DATABASE_URL", pg.PgW.Config().ConnString())
	t.Setenv("CLICKHOUSE_URL", "clickhouse://127.0.0.1:1/default")
	if err := Run(t.Context()); err == nil {
		t.Fatal("Run = nil, want the failure that is the CronJob's only signal")
	}
}

// seedDeletion deletes a new project through the delete request, dated at.
func seedDeletion(t *testing.T, pg *testutil.TestPostgres, at time.Time) string {
	t.Helper()
	ctx := t.Context()
	w := dbwrite.New(pg.PgW)
	orgID, projectID := xid.New().String(), xid.New().String()
	if _, err := w.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: orgID, DisplayName: "Org"}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := w.CreateProject(ctx, dbwrite.CreateProjectParams{ID: projectID, OrgID: orgID, DisplayName: "Doomed"}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := coreprojects.NewService(pg.PgRO, pg.PgW, nil).DeleteProject(ctx, orgID, projectID, "customer test"); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if _, err := pg.PgW.Exec(ctx,
		"update project_deletions set requested_at = $1 where project_id = $2", at, projectID); err != nil {
		t.Fatalf("date the deletion: %v", err)
	}
	return projectID
}

func stalled(t *testing.T, pg *testutil.TestPostgres) bool {
	t.Helper()
	found, err := dbread.New(pg.PgRO).HasStalledProjectDeletions(t.Context())
	if err != nil {
		t.Fatalf("HasStalledProjectDeletions: %v", err)
	}
	return found
}

func status(t *testing.T, pg *testutil.TestPostgres, projectID string) string {
	t.Helper()
	var s string
	if err := pg.PgRO.QueryRow(t.Context(),
		"select status from project_deletions where project_id = $1", projectID).Scan(&s); err != nil {
		t.Fatalf("read status: %v", err)
	}
	return s
}
