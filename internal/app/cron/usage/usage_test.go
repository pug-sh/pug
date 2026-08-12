package usage

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pug-sh/pug/internal/app/cron"
	coreusage "github.com/pug-sh/pug/internal/core/usage"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"github.com/rs/xid"
)

func TestMain(m *testing.M) { testutil.Main(m) }

// newJob builds a job with no ClickHouse, so metering cannot run. Tests that care
// only about the scheduling gates use that to keep the pass cheap.
func newJob(t *testing.T, pg *testutil.TestPostgres) *job {
	t.Helper()
	return &job{
		service:    coreusage.NewService(pg.PgRO, pg.PgW),
		state:      cron.NewState(pg.PgRO, pg.PgW, "usage"),
		pgW:        pg.PgW,
		rescanDays: 2,
	}
}

func seedProject(t *testing.T, pg *testutil.TestPostgres) string {
	t.Helper()
	w := dbwrite.New(pg.PgW)

	org, err := w.CreateOrg(t.Context(), dbwrite.CreateOrgParams{ID: xid.New().String(), DisplayName: "acme"})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	id := xid.New().String()
	project, err := w.CreateProject(t.Context(), dbwrite.CreateProjectParams{
		ID: id, OrgID: org.ID, DisplayName: "project-" + id,
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	return project.ID
}

func countUsageDaily(t *testing.T, pg *testutil.TestPostgres) int {
	t.Helper()
	var n int
	if err := pg.PgRO.QueryRow(t.Context(), "select count(*) from usage_daily").Scan(&n); err != nil {
		t.Fatalf("count usage_daily: %v", err)
	}
	return n
}

// Run's exit code is a CronJob's only success signal, so a pass that could not
// meter has to come back as an error rather than be swallowed.
func TestRunReturnsAFailedPass(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	err := newJob(t, pg).run(t.Context())
	if !errors.Is(err, coreusage.ErrNoMeteringConn) {
		t.Fatalf("run = %v, want ErrNoMeteringConn", err)
	}
}

// A failed task must not stamp itself, or its next attempt waits a full interval.
func TestFailedPassLeavesCronStateUnstamped(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	j := newJob(t, pg)
	if err := j.run(t.Context()); err == nil {
		t.Fatal("run returned nil for a pass that could not meter")
	}

	last, err := j.state.LastRun(t.Context(), taskFullRecompute)
	if err != nil {
		t.Fatalf("LastRun: %v", err)
	}
	if !last.IsZero() {
		t.Errorf("full_recompute stamped %s after a failed pass, want zero", last)
	}
}

// Contention is not failure: another replica holding the lock is doing this work,
// and a CronJob that exited non-zero for it would alert on healthy overlap.
func TestRunSucceedsWhenAnotherHolderHasTheLock(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	ctx := t.Context()

	holder, err := pg.PgW.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder tx: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()

	acquired, err := dbwrite.New(holder).TryCronLock(ctx, int64(cron.LockUsage))
	if err != nil {
		t.Fatalf("take the lock: %v", err)
	}
	if !acquired {
		t.Fatal("could not take the lock to hold it")
	}

	// No ClickHouse: if the lock were ignored, metering would fail and this would
	// return an error instead of skipping.
	if err := newJob(t, pg).run(ctx); err != nil {
		t.Fatalf("run = %v, want nil (the lock holder is doing the work)", err)
	}
}

// The lock is transaction-scoped, so it has to be gone once the pass returns —
// a session-scoped one would ride a pooled connection back and wedge later passes.
func TestLockIsReleasedAfterThePass(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	ctx := t.Context()

	if err := newJob(t, pg).run(ctx); err == nil {
		t.Fatal("run returned nil for a pass that could not meter")
	}

	tx, err := pg.PgW.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	acquired, err := dbwrite.New(tx).TryCronLock(ctx, int64(cron.LockUsage))
	if err != nil {
		t.Fatalf("TryCronLock: %v", err)
	}
	if !acquired {
		t.Error("the lock is still held after the pass returned")
	}
}

// The wide recompute is what catches late arrivals older than the trailing
// rescan. If its gate never opens they are lost for good under a fresh stamp; if
// it never closes, every pass rescans the month. Both are silent in production.
func TestFullRecomputeIsGatedOnCronState(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	ch := testutil.SetupClickHouse(t)
	ctx := t.Context()

	j := newJob(t, pg)
	j.service = j.service.WithClickHouse(ch.Conn)
	j.rescanDays = 1

	// Fixed so the assertions do not depend on today's day-of-month: mid-June, with
	// an event early in the same month — inside the full window, outside the rescan.
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	old := time.Date(2026, 6, 5, 9, 0, 0, 0, time.UTC)

	projectID := seedProject(t, pg)
	testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, "user-1", "$pageview",
		uuid.NewString(), nil, nil, old)

	if err := j.state.MarkRun(ctx, taskFullRecompute, now.Add(-time.Hour)); err != nil {
		t.Fatalf("MarkRun: %v", err)
	}
	if err := j.meter(ctx, now); err != nil {
		t.Fatalf("meter (gated): %v", err)
	}
	if n := countUsageDaily(t, pg); n != 0 {
		t.Fatalf("gated pass metered %d cells outside the rescan window, want 0", n)
	}

	if err := j.state.MarkRun(ctx, taskFullRecompute, now.Add(-25*time.Hour)); err != nil {
		t.Fatalf("MarkRun: %v", err)
	}
	if err := j.meter(ctx, now); err != nil {
		t.Fatalf("meter (due): %v", err)
	}
	if n := countUsageDaily(t, pg); n != 1 {
		t.Errorf("due pass metered %d cells, want 1 (the early-month event)", n)
	}

	// And the gate re-armed, so the next pass is narrow again.
	last, err := j.state.LastRun(ctx, taskFullRecompute)
	if err != nil {
		t.Fatalf("LastRun: %v", err)
	}
	if !last.Equal(now) {
		t.Errorf("full_recompute stamped %s, want %s", last, now)
	}
}

// Both daily sub-tasks are gated on cron_state, not on the schedule: a job firing
// every few minutes must not full-scan the table on every firing.
func TestPruneIsGatedOnCronState(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	ctx := t.Context()
	j := newJob(t, pg)

	projectID := seedProject(t, pg)
	old := coreusage.FloorDayUTC(time.Now().UTC().AddDate(0, 0, -500))
	if err := j.service.RecordDailyUsage(ctx, []coreusage.DailyUsage{
		{Day: old, EventCount: 7, ProjectID: projectID},
	}); err != nil {
		t.Fatalf("RecordDailyUsage: %v", err)
	}

	now := time.Now().UTC()
	if err := j.state.MarkRun(ctx, taskPrune, now.Add(-time.Hour)); err != nil {
		t.Fatalf("MarkRun: %v", err)
	}
	if err := j.prune(ctx, now); err != nil {
		t.Fatalf("prune (gated): %v", err)
	}
	if n := countUsageDaily(t, pg); n != 1 {
		t.Fatalf("gated prune deleted rows: %d remain, want 1", n)
	}

	// Past the interval, the same call does the work.
	if err := j.state.MarkRun(ctx, taskPrune, now.Add(-25*time.Hour)); err != nil {
		t.Fatalf("MarkRun: %v", err)
	}
	if err := j.prune(ctx, now); err != nil {
		t.Fatalf("prune (due): %v", err)
	}
	if n := countUsageDaily(t, pg); n != 0 {
		t.Errorf("due prune left %d rows, want 0", n)
	}
}
