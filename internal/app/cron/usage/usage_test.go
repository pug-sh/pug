package usage

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pug-sh/pug/internal/app/cron"
	coreusage "github.com/pug-sh/pug/internal/core/usage"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"github.com/rs/xid"
	"github.com/sethvargo/go-envconfig"
)

func TestMain(m *testing.M) { testutil.Main(m) }

// newJob builds a job with no ClickHouse, so metering cannot run. Tests that care
// only about the scheduling gates use that to keep the pass cheap.
func newJob(t *testing.T, pg *testutil.TestPostgres) *job {
	t.Helper()
	return &job{
		service:    coreusage.NewService(pg.PgRO, pg.PgW),
		state:      cron.NewState(pg.PgRO, pg.PgW, cron.JobUsage),
		pgW:        pg.PgW,
		rescanDays: 2,
	}
}

func seedProject(t *testing.T, pg *testutil.TestPostgres) string {
	t.Helper()
	_, projectID := seedOrgProject(t, pg)
	return projectID
}

func seedOrgProject(t *testing.T, pg *testutil.TestPostgres) (orgID, projectID string) {
	t.Helper()
	w := dbwrite.New(pg.PgW)

	org, err := w.CreateOrg(t.Context(), dbwrite.CreateOrgParams{ID: xid.New().String(), DisplayName: "acme"})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	// Backdated to the 1st so the org's quota window is the calendar month these
	// assertions assume — an anchor derives from create_time, so an org created
	// "now" would shift every expected bound with the suite's run date.
	testutil.SetOrgCreateTime(t, pg.PgW, org.ID, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	id := xid.New().String()
	project, err := w.CreateProject(t.Context(), dbwrite.CreateProjectParams{
		ID: id, OrgID: org.ID, DisplayName: "project-" + id,
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	return org.ID, project.ID
}

func countUsageDaily(t *testing.T, pg *testutil.TestPostgres) int {
	t.Helper()
	var n int
	if err := pg.PgRO.QueryRow(t.Context(), "select count(*) from usage_daily").Scan(&n); err != nil {
		t.Fatalf("count usage_daily: %v", err)
	}
	return n
}

func usageDayCount(t *testing.T, pg *testutil.TestPostgres, projectID string, day time.Time) (int64, bool) {
	t.Helper()
	var n int64
	err := pg.PgRO.QueryRow(t.Context(),
		"select event_count from usage_daily where project_id = $1 and day = $2",
		projectID, day).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false
	}
	if err != nil {
		t.Fatalf("read usage_daily cell: %v", err)
	}
	return n, true
}

// The rescan window's floor has to land on a UTC midnight. Drop the FloorDayUTC
// and `from` becomes a mid-day instant: MeterWindow then counts only the part of
// the boundary day after it, while DeleteUnmeteredDays still reconciles the whole
// day, so that day's stored count silently becomes a function of what time the
// CronJob happened to fire.
//
// Tests place their events straddling the boundary for that reason — an event
// well inside the window cannot tell the two apart.
func TestMeterCountsTheWholeBoundaryDay(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	ch := testutil.SetupClickHouse(t)
	ctx := t.Context()

	j := newJob(t, pg)
	j.service = j.service.WithClickHouse(ch.Conn)
	j.rescanDays = 1

	// Fixed so the assertions do not depend on today. now is mid-day, so a floored
	// window starts at 06-19T00:00Z and an unfloored one at 06-19T12:00Z.
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	boundary := time.Date(2026, 6, 19, 0, 0, 0, 0, time.UTC)

	projectID := seedProject(t, pg)
	for _, at := range []time.Time{
		time.Date(2026, 6, 19, 0, 30, 0, 0, time.UTC),  // inside the day, before an unfloored `from`
		time.Date(2026, 6, 19, 18, 0, 0, 0, time.UTC),  // inside the day, after it
		time.Date(2026, 6, 18, 23, 30, 0, 0, time.UTC), // the day before: outside either way
	} {
		testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, "user-1", "$pageview",
			uuid.NewString(), nil, nil, at)
	}

	// Keep the pass narrow; a full recompute would widen `from` to the 1st and hide
	// the boundary entirely.
	if err := j.state.MarkRun(ctx, taskFullRecompute, now.Add(-time.Hour)); err != nil {
		t.Fatalf("MarkRun: %v", err)
	}
	if err := j.meter(ctx, now); err != nil {
		t.Fatalf("meter: %v", err)
	}

	got, ok := usageDayCount(t, pg, projectID, boundary)
	if !ok {
		t.Fatal("no cell stored for the boundary day")
	}
	if got != 2 {
		t.Errorf("boundary day = %d, want 2 — the window starts mid-day instead of at UTC midnight", got)
	}
	if _, ok := usageDayCount(t, pg, projectID, boundary.AddDate(0, 0, -1)); ok {
		t.Error("metered the day before the window")
	}
}

// A ClickHouse reporting projects this Postgres has never heard of is a pair
// pointed at different environments. Every upsert then writes nothing and reports
// success, so reconciling would delete the cells earlier correct passes stored and
// stamp every org with a zero.
func TestMeterRefusesToReconcileWhenNoProjectIsKnown(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	ch := testutil.SetupClickHouse(t)
	ctx := t.Context()

	j := newJob(t, pg)
	j.service = j.service.WithClickHouse(ch.Conn)
	j.rescanDays = 2

	now := time.Now().UTC()
	day := coreusage.FloorDayUTC(now)

	// A real project with a stored cell, standing in for what earlier passes wrote.
	orgID, projectID := seedOrgProject(t, pg)
	if err := j.service.RecordDailyUsage(ctx, []coreusage.DailyUsage{
		{Day: day, EventCount: 42, ProjectID: projectID},
	}); err != nil {
		t.Fatalf("RecordDailyUsage: %v", err)
	}

	// Events for a project id Postgres does not know — the other environment.
	stranger := xid.New().String()
	testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), stranger, "user-1", "$pageview",
		uuid.NewString(), nil, nil, now.Add(-time.Hour))

	if err := j.meter(ctx, now); err != nil {
		t.Fatalf("meter = %v, want nil (a bad pairing must not thrash the CronJob)", err)
	}

	if got, ok := usageDayCount(t, pg, projectID, day); !ok || got != 42 {
		t.Errorf("stored cell = (%d, %t), want (42, true) — the pass reconciled against a foreign ClickHouse", got, ok)
	}
	usage, err := j.service.GetPeriodUsage(ctx, orgID, mustPeriodStart(now))
	if err != nil {
		t.Fatalf("GetPeriodUsage: %v", err)
	}
	if usage.Counted {
		t.Error("refreshed a period from a read it could not attribute to any known project")
	}
}

func mustPeriodStart(now time.Time) time.Time {
	start, _ := coreusage.PeriodFor(now, 1)
	return start
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

	// No ClickHouse: if the lock were ignored, metering would fail with
	// ErrNoMeteringConn instead, so ErrLockHeld proves the lock was respected
	// rather than merely that nothing went wrong.
	if err := newJob(t, pg).run(ctx); !errors.Is(err, cron.ErrLockHeld) {
		t.Fatalf("run = %v, want ErrLockHeld (the lock holder is doing the work)", err)
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

// An empty read *over days pug has already counted* is treated as a bad read, not
// as an emptied window: the stored days stand, and the gate stays open so the next
// pass retries the wide window instead of waiting out the interval. The accepted
// cost is a genuinely emptied window keeping a stale count — see usage.md §8.
//
// The scope matters: with nothing stored the same empty read is treated as idle
// and every period is refreshed as normal, which
// TestIdleEmptyReadStillRefreshesTheOrgPeriod covers.
func TestEmptyMeterReadKeepsStoredDaysAndLeavesTheGateOpen(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	ch := testutil.SetupClickHouse(t)
	ctx := t.Context()

	j := newJob(t, pg)
	j.service = j.service.WithClickHouse(ch.Conn)

	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	projectID := seedProject(t, pg)
	if err := j.service.RecordDailyUsage(ctx, []coreusage.DailyUsage{
		{Day: time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC), EventCount: 4, ProjectID: projectID},
	}); err != nil {
		t.Fatalf("RecordDailyUsage: %v", err)
	}

	if err := j.meter(ctx, now); err != nil {
		t.Fatalf("meter: %v", err)
	}
	if n := countUsageDaily(t, pg); n != 1 {
		t.Errorf("usage_daily has %d rows, want 1: an empty read wiped the window", n)
	}

	last, err := j.state.LastRun(ctx, taskFullRecompute)
	if err != nil {
		t.Fatalf("LastRun: %v", err)
	}
	if !last.IsZero() {
		t.Errorf("full_recompute stamped %s on an empty read, deferring the next wide window", last)
	}
}

// Both daily sub-tasks are gated on cron_state, not on the schedule: the
// retention boundary only moves once a day, so a job firing every few minutes
// would re-scan the same range to delete nothing.
//
// Seeds a row on each side of the boundary so the "due" leg proves the prune
// deleted the right row rather than merely emptying the table.
func TestPruneIsGatedOnCronState(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	ctx := t.Context()
	j := newJob(t, pg)

	projectID := seedProject(t, pg)
	now := time.Now().UTC()
	old := coreusage.FloorDayUTC(now.AddDate(0, 0, -500))
	recent := coreusage.FloorDayUTC(now.AddDate(0, 0, -10))
	if err := j.service.RecordDailyUsage(ctx, []coreusage.DailyUsage{
		{Day: old, EventCount: 7, ProjectID: projectID},
		{Day: recent, EventCount: 11, ProjectID: projectID},
	}); err != nil {
		t.Fatalf("RecordDailyUsage: %v", err)
	}

	if err := j.state.MarkRun(ctx, taskPrune, now.Add(-time.Hour)); err != nil {
		t.Fatalf("MarkRun: %v", err)
	}
	if err := j.prune(ctx, now); err != nil {
		t.Fatalf("prune (gated): %v", err)
	}
	if n := countUsageDaily(t, pg); n != 2 {
		t.Fatalf("gated prune deleted rows: %d remain, want 2", n)
	}

	// Past the interval, the same call does the work — and only the work.
	if err := j.state.MarkRun(ctx, taskPrune, now.Add(-25*time.Hour)); err != nil {
		t.Fatalf("MarkRun: %v", err)
	}
	if err := j.prune(ctx, now); err != nil {
		t.Fatalf("prune (due): %v", err)
	}
	if n := countUsageDaily(t, pg); n != 1 {
		t.Errorf("due prune left %d rows, want 1 (the in-retention day)", n)
	}
}

// A narrow pass must reconcile only the window it actually metered. The drop is
// scoped by day range alone -- no org, no project -- so a `from` wider than the
// metered window silently deletes older days for every org in the deployment.
func TestNarrowPassDoesNotReconcileBeyondItsWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	ch := testutil.SetupClickHouse(t)
	ctx := t.Context()

	j := newJob(t, pg)
	j.service = j.service.WithClickHouse(ch.Conn)

	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	projectID := seedProject(t, pg)

	// Well outside the 2-day rescan, so this pass never meters it and the drop is
	// the only thing that could remove it.
	if err := j.service.RecordDailyUsage(ctx, []coreusage.DailyUsage{
		{Day: time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC), EventCount: 4, ProjectID: projectID},
	}); err != nil {
		t.Fatalf("RecordDailyUsage: %v", err)
	}
	// Inside it, so the read comes back non-empty and the drop really runs.
	testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, "user-1", "$pageview",
		uuid.NewString(), nil, nil, time.Date(2026, 6, 19, 9, 0, 0, 0, time.UTC))

	// Shut the full-recompute gate so `from` stays at now-2d rather than widening
	// to the month start, which would legitimately cover the June 5 cell.
	if err := j.state.MarkRun(ctx, taskFullRecompute, now.Add(-time.Hour)); err != nil {
		t.Fatalf("MarkRun: %v", err)
	}
	if err := j.meter(ctx, now); err != nil {
		t.Fatalf("meter: %v", err)
	}

	if n := countUsageDaily(t, pg); n != 2 {
		t.Errorf("usage_daily has %d rows, want 2: the narrow pass reconciled outside its own window", n)
	}
}

// usage_periods is the number the dashboard actually renders. Day cells can be
// written perfectly and the headline still be wrong for every org, so assert the
// period the pass stored, not just the cells behind it.
func TestMeterRefreshesTheOrgPeriod(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	ch := testutil.SetupClickHouse(t)
	ctx := t.Context()

	j := newJob(t, pg)
	j.service = j.service.WithClickHouse(ch.Conn)

	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	orgID, projectID := seedOrgProject(t, pg)
	for range 3 {
		testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, "user-1", "$pageview",
			uuid.NewString(), nil, nil, now.Add(-time.Hour))
	}

	if err := j.meter(ctx, now); err != nil {
		t.Fatalf("meter: %v", err)
	}

	start, _ := coreusage.PeriodFor(now, 1)
	got, err := j.service.GetPeriodUsage(ctx, orgID, start)
	if err != nil {
		t.Fatalf("GetPeriodUsage: %v", err)
	}
	if got.EventCount != 3 {
		t.Errorf("period event_count = %d, want 3", got.EventCount)
	}
	if got.UsageComputedAt.IsZero() {
		t.Error("usage_computed_at unset after a good pass, so the dashboard reads 'never metered'")
	}
}

// An empty read over days pug has already counted is a contradiction -- ClickHouse
// stopped returning history that exists. Advancing usage_computed_at there would
// report fresh-but-frozen numbers and hide the only layer that catches it, since
// the query itself succeeded and the pass exits 0.
func TestSuspiciousEmptyReadRefreshesNoPeriod(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	ch := testutil.SetupClickHouse(t)
	ctx := t.Context()

	j := newJob(t, pg)
	j.service = j.service.WithClickHouse(ch.Conn)

	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	orgID, projectID := seedOrgProject(t, pg)
	// Stored inside the metered window, with no ClickHouse events behind it.
	if err := j.service.RecordDailyUsage(ctx, []coreusage.DailyUsage{
		{Day: time.Date(2026, 6, 19, 0, 0, 0, 0, time.UTC), EventCount: 4, ProjectID: projectID},
	}); err != nil {
		t.Fatalf("RecordDailyUsage: %v", err)
	}

	if err := j.meter(ctx, now); err != nil {
		t.Fatalf("meter: %v", err)
	}

	start, _ := coreusage.PeriodFor(now, 1)
	got, err := j.service.GetPeriodUsage(ctx, orgID, start)
	if err != nil {
		t.Fatalf("GetPeriodUsage: %v", err)
	}
	if !got.UsageComputedAt.IsZero() {
		t.Error("a suspicious empty read stamped usage_computed_at, so the staleness alert can never fire")
	}
	if n := countUsageDaily(t, pg); n != 1 {
		t.Errorf("usage_daily has %d rows, want 1: the empty read wiped the window", n)
	}
}

// The same empty read on a deployment that has never stored anything is just an
// idle one, and must still refresh -- that is what makes an eventless org read as
// a metered zero rather than as "never metered".
func TestIdleEmptyReadStillRefreshesTheOrgPeriod(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	ch := testutil.SetupClickHouse(t)
	ctx := t.Context()

	j := newJob(t, pg)
	j.service = j.service.WithClickHouse(ch.Conn)

	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	orgID, _ := seedOrgProject(t, pg)

	if err := j.meter(ctx, now); err != nil {
		t.Fatalf("meter: %v", err)
	}

	start, _ := coreusage.PeriodFor(now, 1)
	got, err := j.service.GetPeriodUsage(ctx, orgID, start)
	if err != nil {
		t.Fatalf("GetPeriodUsage: %v", err)
	}
	if got.UsageComputedAt.IsZero() {
		t.Error("an idle deployment was left unmetered, so an eventless org reads 'never metered' forever")
	}
	if got.EventCount != 0 {
		t.Errorf("period event_count = %d, want 0", got.EventCount)
	}
}

// The clamp lived inside Run, which needs env vars and real pools, so nothing
// exercised it. A negative window puts `from` in the future: every read comes back
// empty and the pass meters nothing, quietly, forever.
// rescanDays is tested in isolation below, which cannot catch a typo'd struct tag
// or a Run that forgets to route the config through the clamp — both of which
// silently disable the documented .env knob, or ship a negative window that meters
// nothing forever.
func TestConfigReadsTheDocumentedEnvVar(t *testing.T) {
	for _, tc := range []struct {
		name, env string
		want      int
	}{
		{"documented default", "2", 2},
		{"unset falls back", "", defaultRescanDays},
		{"negative is clamped on the way through", "-5", defaultRescanDays},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{}
			if tc.env != "" {
				env["PUG_USAGE_RESCAN_DAYS"] = tc.env
			}

			var cfg Config
			if err := envconfig.ProcessWith(t.Context(), &envconfig.Config{
				Target:   &cfg,
				Lookuper: envconfig.MapLookuper(env),
			}); err != nil {
				t.Fatalf("ProcessWith: %v", err)
			}
			if got := rescanDays(t.Context(), cfg.RescanDays); got != tc.want {
				t.Errorf("PUG_USAGE_RESCAN_DAYS=%q resolved to %d, want %d", tc.env, got, tc.want)
			}
		})
	}
}

func TestRescanDaysClampsToTheDefault(t *testing.T) {
	for _, tc := range []struct {
		name     string
		in, want int
	}{
		{"unset falls back", 0, defaultRescanDays},
		{"negative falls back", -3, defaultRescanDays},
		{"positive is honoured", 5, 5},
		{"at the retention window is honoured", maxRescanDays, maxRescanDays},
		// Past retention the meter re-inserts day cells the same pass's prune then
		// deletes, forever, and the ClickHouse scan stops pruning partitions.
		{"absurd clamps to retention", 100_000, maxRescanDays},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rescanDays(t.Context(), tc.in); got != tc.want {
				t.Errorf("rescanDays(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}
