package retention

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/xid"

	"github.com/pug-sh/pug/internal/app/cron"
	"github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/core/billing/entitlement"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

func TestMain(m *testing.M) { testutil.Main(m) }

// Off by default, it only logs; on, it queues the delete.
func TestRunDeletesOnlyWithTheSwitchOn(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	ch := testutil.SetupClickHouse(t)
	t.Setenv("DATABASE_URL", pg.PgW.Config().ConnString())
	t.Setenv("CLICKHOUSE_URL", ch.URL)
	projectID := seedProject(t, pg, ch)

	t.Setenv("PUG_RETENTION_ENABLED", "false")
	if err := Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := deletes(t, ch, projectID); n != 0 {
		t.Fatalf("the switch off queued %d deletes", n)
	}

	t.Setenv("PUG_RETENTION_ENABLED", "true")
	if err := Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := deletes(t, ch, projectID); n == 0 {
		t.Fatal("the switch on queued no delete")
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
	t.Setenv("PUG_RETENTION_ENABLED", "true")
	projectID := seedProject(t, pg, ch)

	ctx := t.Context()
	holder, err := pg.PgW.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder tx: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if acquired, err := dbwrite.New(holder).TryCronLock(ctx, int64(cron.LockRetention)); err != nil || !acquired {
		t.Fatalf("TryCronLock = %t, %v", acquired, err)
	}

	if err := Run(ctx); err != nil {
		t.Fatalf("Run = %v, want nil while another pass holds the lock", err)
	}
	if n := deletes(t, ch, projectID); n != 0 {
		t.Errorf("queued %d deletes beside the lock holder", n)
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

// seedProject gives an org a 90-day length that already applies, and a project
// with an event long past it.
func seedProject(t *testing.T, pg *testutil.TestPostgres, ch *testutil.TestClickHouse) string {
	t.Helper()
	ctx := t.Context()
	w := dbwrite.New(pg.PgW)
	orgID, projectID := xid.New().String(), xid.New().String()
	if _, err := w.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: orgID, DisplayName: "Org"}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := w.CreateProject(ctx, dbwrite.CreateProjectParams{ID: projectID, OrgID: orgID, DisplayName: "P"}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	ents, err := entitlement.NewService(pg.PgW, pg.PgW, billing.Config{})
	if err != nil {
		t.Fatalf("entitlement.NewService: %v", err)
	}
	days := int64(90)
	if _, err := ents.SetPlan(ctx, orgID, "ops/test", entitlement.Change{PlanSlug: entitlement.SlugFree, RetentionDays: &days}); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}
	if _, err := pg.PgW.Exec(ctx, "insert into retention_state (days, org_id) values ($1, $2)", days, orgID); err != nil {
		t.Fatalf("seed retention state: %v", err)
	}
	testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, "user-1", "page_view", uuid.NewString(),
		nil, nil, time.Now().AddDate(-1, 0, 0))
	return projectID
}

func deletes(t *testing.T, ch *testutil.TestClickHouse, projectID string) uint64 {
	t.Helper()
	var n uint64
	if err := ch.Conn.QueryRow(t.Context(),
		"SELECT count() FROM system.mutations WHERE database = currentDatabase() AND position(command, ?) > 0",
		projectID).Scan(&n); err != nil {
		t.Fatalf("count deletes: %v", err)
	}
	return n
}
