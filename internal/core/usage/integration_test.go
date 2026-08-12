package usage_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
	coreusage "github.com/pug-sh/pug/internal/core/usage"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"github.com/rs/xid"
)

// fixture is one test's org + project, with the service under test.
type fixture struct {
	svc       *coreusage.Service
	w         *dbwrite.Queries
	orgID     string
	projectID string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pg := testutil.SetupPostgres(t)
	f := &fixture{
		svc: coreusage.NewService(pg.PgRO, pg.PgW),
		w:   dbwrite.New(pg.PgW),
	}
	f.orgID, f.projectID = seedOrgProject(t, f.w)
	return f
}

func seedOrgProject(t *testing.T, w *dbwrite.Queries) (orgID, projectID string) {
	t.Helper()

	org, err := w.CreateOrg(t.Context(), dbwrite.CreateOrgParams{
		ID:          xid.New().String(),
		DisplayName: "acme",
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	return org.ID, seedProject(t, w, org.ID)
}

func seedProject(t *testing.T, w *dbwrite.Queries, orgID string) string {
	t.Helper()

	id := xid.New().String()
	// display_name is unique per org, so it cannot be a constant.
	project, err := w.CreateProject(t.Context(), dbwrite.CreateProjectParams{
		ID:          id,
		OrgID:       orgID,
		DisplayName: "project-" + id,
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	return project.ID
}

func insertEvent(t *testing.T, conn driver.Conn, projectID, eventID string, occurTime time.Time) {
	t.Helper()
	testutil.InsertEvent(t.Context(), t, conn, eventID, projectID, "user-1", "$pageview",
		uuid.NewString(), nil, nil, occurTime)
}

func TestMeteringCountsDistinctEventsOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ch := testutil.SetupClickHouse(t)
	svc := f.svc.WithClickHouse(ch.Conn)
	ctx := t.Context()

	day := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	duplicated := uuid.NewString()

	// The same event_id delivered twice within the minute: the storage key dedups
	// it on merge, and uniqExact gives the same answer whether or not that merge
	// has happened.
	insertEvent(t, ch.Conn, f.projectID, duplicated, day)
	insertEvent(t, ch.Conn, f.projectID, duplicated, day.Add(10*time.Second))
	insertEvent(t, ch.Conn, f.projectID, uuid.NewString(), day.Add(time.Minute))

	usage, err := svc.MeterWindow(ctx, day.AddDate(0, 0, -1), day.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("MeterWindow: %v", err)
	}
	if len(usage) != 1 {
		t.Fatalf("got %d cells, want 1: %+v", len(usage), usage)
	}
	if usage[0].EventCount != 2 {
		t.Errorf("event_count = %d, want 2 (the duplicate counts once)", usage[0].EventCount)
	}
}

// Pins the day boundary against a future edit. It cannot catch the server-timezone
// hazard the query's explicit toDate(_, 'UTC') guards — the test container runs
// UTC, where bare and explicit toDate agree.
func TestMeteringCutsDaysAtUTCMidnight(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ch := testutil.SetupClickHouse(t)
	svc := f.svc.WithClickHouse(ch.Conn)

	lateOnTheTenth := time.Date(2026, 7, 10, 23, 50, 0, 0, time.UTC)
	earlyOnTheEleventh := time.Date(2026, 7, 11, 0, 10, 0, 0, time.UTC)
	insertEvent(t, ch.Conn, f.projectID, uuid.NewString(), lateOnTheTenth)
	insertEvent(t, ch.Conn, f.projectID, uuid.NewString(), earlyOnTheEleventh)

	usage, err := svc.MeterWindow(t.Context(),
		lateOnTheTenth.AddDate(0, 0, -1), earlyOnTheEleventh.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("MeterWindow: %v", err)
	}

	// MeterWindow has no ORDER BY, so assert on the set of days.
	days := map[time.Time]int64{}
	for _, u := range usage {
		days[u.Day] = u.EventCount
	}
	want := map[time.Time]int64{
		time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC): 1,
		time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC): 1,
	}
	if len(days) != len(want) {
		t.Fatalf("got %+v, want one cell either side of UTC midnight", usage)
	}
	for day, count := range want {
		if days[day] != count {
			t.Errorf("%s = %d, want %d", day, days[day], count)
		}
	}
}

func TestMeteringBucketsEventsIntoTheRightMonth(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ch := testutil.SetupClickHouse(t)
	svc := f.svc.WithClickHouse(ch.Conn)
	ctx := t.Context()

	julyEnd := time.Date(2026, 7, 31, 23, 30, 0, 0, time.UTC)
	augStart := time.Date(2026, 8, 1, 0, 30, 0, 0, time.UTC)
	insertEvent(t, ch.Conn, f.projectID, uuid.NewString(), julyEnd)
	insertEvent(t, ch.Conn, f.projectID, uuid.NewString(), augStart)
	insertEvent(t, ch.Conn, f.projectID, uuid.NewString(), augStart.Add(time.Hour))

	usage, err := svc.MeterWindow(ctx, julyEnd.AddDate(0, 0, -1), augStart.AddDate(0, 0, 2))
	if err != nil {
		t.Fatalf("MeterWindow: %v", err)
	}
	if err := svc.RecordDailyUsage(ctx, usage); err != nil {
		t.Fatalf("RecordDailyUsage: %v", err)
	}

	julyStart, julyStop := coreusage.CalendarMonth(julyEnd)
	july, err := svc.RefreshPeriodUsage(ctx, f.orgID, julyStart, julyStop)
	if err != nil {
		t.Fatalf("RefreshPeriodUsage (july): %v", err)
	}
	if july != 1 {
		t.Errorf("july = %d, want 1", july)
	}

	augStartOfMonth, augStop := coreusage.CalendarMonth(augStart)
	august, err := svc.RefreshPeriodUsage(ctx, f.orgID, augStartOfMonth, augStop)
	if err != nil {
		t.Fatalf("RefreshPeriodUsage (august): %v", err)
	}
	if august != 2 {
		t.Errorf("august = %d, want 2", august)
	}
}

func TestLateArrivalInsideTheTrailingWindowUpdatesItsDay(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ch := testutil.SetupClickHouse(t)
	svc := f.svc.WithClickHouse(ch.Conn)
	ctx := t.Context()

	// Pinned mid-month: the assertions sum over CalendarMonth(now) while the event
	// lands a day earlier, which on the 1st would fall in the previous month.
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	yesterday := now.AddDate(0, 0, -1).Truncate(time.Hour)
	insertEvent(t, ch.Conn, f.projectID, uuid.NewString(), yesterday)

	rescan := func() int64 {
		t.Helper()
		usage, err := svc.MeterWindow(ctx, now.AddDate(0, 0, -2).Truncate(24*time.Hour), now.Add(time.Hour))
		if err != nil {
			t.Fatalf("MeterWindow: %v", err)
		}
		if err := svc.RecordDailyUsage(ctx, usage); err != nil {
			t.Fatalf("RecordDailyUsage: %v", err)
		}
		start, end := coreusage.CalendarMonth(now)
		total, err := svc.RefreshPeriodUsage(ctx, f.orgID, start, end)
		if err != nil {
			t.Fatalf("RefreshPeriodUsage: %v", err)
		}
		return total
	}

	if got := rescan(); got != 1 {
		t.Fatalf("first pass = %d, want 1", got)
	}

	// A late arrival for a day already metered: the trailing rescan overwrites the
	// day row rather than adding to it, so the count is corrected, not doubled.
	insertEvent(t, ch.Conn, f.projectID, uuid.NewString(), yesterday.Add(time.Minute))
	if got := rescan(); got != 2 {
		t.Errorf("after the late arrival = %d, want 2", got)
	}
}

func TestRefreshPeriodUsageStoresTheSingleRowRead(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ch := testutil.SetupClickHouse(t)
	svc := f.svc.WithClickHouse(ch.Conn)
	ctx := t.Context()

	// Pinned mid-month: the event lands two hours earlier, which in the first two
	// hours of the 1st would fall outside CalendarMonth(now).
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	insertEvent(t, ch.Conn, f.projectID, uuid.NewString(), now.Add(-2*time.Hour))

	// Before the meter runs there is no row, which reads as zero usage rather than
	// an error — the org has simply not been counted yet. The zero stamp is what
	// tells the dashboard the difference.
	start, end := coreusage.CalendarMonth(now)
	usage, err := svc.GetPeriodUsage(ctx, f.orgID, start)
	if err != nil {
		t.Fatalf("GetPeriodUsage: %v", err)
	}
	if usage.EventCount != 0 || !usage.UsageComputedAt.IsZero() {
		t.Errorf("got %+v, want a zero-valued reading", usage)
	}

	cells, err := svc.MeterWindow(ctx, start, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("MeterWindow: %v", err)
	}
	if err := svc.RecordDailyUsage(ctx, cells); err != nil {
		t.Fatalf("RecordDailyUsage: %v", err)
	}
	total, err := svc.RefreshPeriodUsage(ctx, f.orgID, start, end)
	if err != nil {
		t.Fatalf("RefreshPeriodUsage: %v", err)
	}
	// The stored row and the returned total are one statement, so a caller that
	// trusts the return value can never disagree with a caller that re-reads.
	if total != 1 {
		t.Errorf("RefreshPeriodUsage returned %d, want 1", total)
	}

	usage, err = svc.GetPeriodUsage(ctx, f.orgID, start)
	if err != nil {
		t.Fatalf("GetPeriodUsage: %v", err)
	}
	if usage.EventCount != 1 {
		t.Errorf("event_count = %d, want 1", usage.EventCount)
	}
	if usage.UsageComputedAt.IsZero() {
		t.Error("usage_computed_at is zero; every surface must be able to date-stamp the number")
	}
}

// MeterWindow is global — no project or org filter — and RecordDailyUsage resolves
// project->org in SQL. Two orgs metered in one pass must not bleed into each
// other's period sum.
func TestMeteringKeepsTwoOrgsApart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ch := testutil.SetupClickHouse(t)
	svc := f.svc.WithClickHouse(ch.Conn)
	ctx := t.Context()

	otherOrgID, otherProjectID := seedOrgProject(t, f.w)

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	insertEvent(t, ch.Conn, f.projectID, uuid.NewString(), now.Add(-time.Hour))
	for range 3 {
		insertEvent(t, ch.Conn, otherProjectID, uuid.NewString(), now.Add(-time.Hour))
	}

	start, end := coreusage.CalendarMonth(now)
	usage, err := svc.MeterWindow(ctx, start, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("MeterWindow: %v", err)
	}
	if err := svc.RecordDailyUsage(ctx, usage); err != nil {
		t.Fatalf("RecordDailyUsage: %v", err)
	}

	mine, err := svc.RefreshPeriodUsage(ctx, f.orgID, start, end)
	if err != nil {
		t.Fatalf("RefreshPeriodUsage (first org): %v", err)
	}
	if mine != 1 {
		t.Errorf("first org = %d, want 1", mine)
	}
	theirs, err := svc.RefreshPeriodUsage(ctx, otherOrgID, start, end)
	if err != nil {
		t.Fatalf("RefreshPeriodUsage (second org): %v", err)
	}
	if theirs != 3 {
		t.Errorf("second org = %d, want 3", theirs)
	}
}

// The daily series is the chart's data source, so it has to come back at the
// grain it was stored: one cell per project per day, oldest first.
func TestListDailyUsageReturnsThePerProjectSeries(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ctx := t.Context()

	// A second project in the SAME org: the series has to span an org's projects,
	// not just the one the events happened to land in.
	secondProjectID := seedProject(t, f.w, f.orgID)
	// And a foreign org's cell on the same day, which must never appear.
	_, foreignProjectID := seedOrgProject(t, f.w)

	day1 := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	day2 := day1.AddDate(0, 0, 1)
	if err := f.svc.RecordDailyUsage(ctx, []coreusage.DailyUsage{
		{Day: day2, EventCount: 5, ProjectID: f.projectID},
		{Day: day1, EventCount: 3, ProjectID: f.projectID},
		{Day: day1, EventCount: 2, ProjectID: secondProjectID},
		{Day: day1, EventCount: 99, ProjectID: foreignProjectID},
	}); err != nil {
		t.Fatalf("RecordDailyUsage: %v", err)
	}

	daily, err := f.svc.ListDailyUsage(ctx, f.orgID, day1, day2.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("ListDailyUsage: %v", err)
	}
	if len(daily) != 3 {
		t.Fatalf("got %d cells, want 3 (the foreign org's cell must not appear): %+v", len(daily), daily)
	}
	if !daily[0].Day.Equal(day1) || !daily[2].Day.Equal(day2) {
		t.Errorf("series is not oldest-first: %+v", daily)
	}
	for _, d := range daily {
		if d.ProjectID == foreignProjectID {
			t.Fatalf("another org's project leaked into the series: %+v", d)
		}
	}

	// A window that excludes day1 excludes both of its cells, half-open at the top.
	daily, err = f.svc.ListDailyUsage(ctx, f.orgID, day2, day2.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("ListDailyUsage (narrowed): %v", err)
	}
	if len(daily) != 1 || daily[0].EventCount != 5 {
		t.Errorf("narrowed window = %+v, want just day2's single cell", daily)
	}
}

// The meter is driven from orgs, not from usage rows, so an org that has never
// sent an event still gets a period row — the dashboard then reads a metered zero
// with a real timestamp rather than "never metered".
func TestOrgPeriodsCoversAnOrgWithNoEvents(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ctx := t.Context()

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	periods, err := f.svc.OrgPeriods(ctx, now)
	if err != nil {
		t.Fatalf("OrgPeriods: %v", err)
	}
	if len(periods) != 1 || periods[0].OrgID != f.orgID {
		t.Fatalf("got %+v, want the one seeded org", periods)
	}
	wantStart, wantEnd := coreusage.CalendarMonth(now)
	if !periods[0].Start.Equal(wantStart) || !periods[0].End.Equal(wantEnd) {
		t.Errorf("period = [%s, %s), want [%s, %s)",
			periods[0].Start, periods[0].End, wantStart, wantEnd)
	}

	if _, err := f.svc.RefreshPeriodUsage(ctx, f.orgID, wantStart, wantEnd); err != nil {
		t.Fatalf("RefreshPeriodUsage: %v", err)
	}
	usage, err := f.svc.GetPeriodUsage(ctx, f.orgID, wantStart)
	if err != nil {
		t.Fatalf("GetPeriodUsage: %v", err)
	}
	if usage.EventCount != 0 {
		t.Errorf("event_count = %d, want 0", usage.EventCount)
	}
	if usage.UsageComputedAt.IsZero() {
		t.Error("usage_computed_at is zero; a metered zero must be distinguishable from an unmetered org")
	}
}

// Counts have to converge downward too: GDPR erasure hard-deletes events, so a
// day that loses its last event drops out of the metered set entirely and an
// upsert-only pass would leave the old count standing.
func TestDeleteUnmeteredDaysDropsCellsWithNoEventsLeft(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ctx := t.Context()

	erased := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	kept := erased.AddDate(0, 0, 1)
	outside := erased.AddDate(0, 0, -5)
	if err := f.svc.RecordDailyUsage(ctx, []coreusage.DailyUsage{
		{Day: outside, EventCount: 1, ProjectID: f.projectID},
		{Day: erased, EventCount: 4, ProjectID: f.projectID},
		{Day: kept, EventCount: 6, ProjectID: f.projectID},
	}); err != nil {
		t.Fatalf("RecordDailyUsage: %v", err)
	}

	// A re-meter of [erased, kept] that now sees only `kept` — `erased` was emptied.
	remetered := []coreusage.DailyUsage{{Day: kept, EventCount: 6, ProjectID: f.projectID}}
	dropped, err := f.svc.DeleteUnmeteredDays(ctx, remetered, erased, kept.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("DeleteUnmeteredDays: %v", err)
	}
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}

	daily, err := f.svc.ListDailyUsage(ctx, f.orgID, outside, kept.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("ListDailyUsage: %v", err)
	}
	if len(daily) != 2 {
		t.Fatalf("got %d cells, want 2: %+v", len(daily), daily)
	}
	// The cell before the window is untouched — a pass only reconciles what it met.
	if !daily[0].Day.Equal(outside) || !daily[1].Day.Equal(kept) {
		t.Errorf("wrong cells survived: %+v", daily)
	}
}

// A month rollover leaves the new period rowless until the next pass. That is a
// metered org with nothing counted yet, not an unmetered one — reporting it as
// "never metered" would blank every dashboard on the 1st.
func TestPeriodNotYetMeteredKeepsTheLastStamp(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ctx := t.Context()

	july := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	julyStart, julyEnd := coreusage.CalendarMonth(july)
	if err := f.svc.RecordDailyUsage(ctx, []coreusage.DailyUsage{
		{Day: coreusage.FloorDayUTC(july), EventCount: 12, ProjectID: f.projectID},
	}); err != nil {
		t.Fatalf("RecordDailyUsage: %v", err)
	}
	if _, err := f.svc.RefreshPeriodUsage(ctx, f.orgID, julyStart, julyEnd); err != nil {
		t.Fatalf("RefreshPeriodUsage: %v", err)
	}

	// August, before the first pass of the month has run.
	augStart, _ := coreusage.CalendarMonth(july.AddDate(0, 1, 0))
	usage, err := f.svc.GetPeriodUsage(ctx, f.orgID, augStart)
	if err != nil {
		t.Fatalf("GetPeriodUsage: %v", err)
	}
	if usage.EventCount != 0 {
		t.Errorf("event_count = %d, want 0 for a period with no rows yet", usage.EventCount)
	}
	if usage.UsageComputedAt.IsZero() {
		t.Error("usage_computed_at is zero, so a metered org reads as never metered at every month rollover")
	}
}

// RefreshUsagePeriod is deliberately ungated where UpsertUsageDaily is not: the
// stamp has to advance every pass, idle org or not, or a stale-stamp alert cannot
// tell a quiet org from a dead meter.
func TestRefreshPeriodAdvancesTheStampForAnIdleOrg(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ctx := t.Context()
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	start, end := coreusage.CalendarMonth(now)

	if err := f.svc.RecordDailyUsage(ctx, []coreusage.DailyUsage{
		{Day: coreusage.FloorDayUTC(now), EventCount: 3, ProjectID: f.projectID},
	}); err != nil {
		t.Fatalf("RecordDailyUsage: %v", err)
	}
	if _, err := f.svc.RefreshPeriodUsage(ctx, f.orgID, start, end); err != nil {
		t.Fatalf("RefreshPeriodUsage: %v", err)
	}
	first, err := f.svc.GetPeriodUsage(ctx, f.orgID, start)
	if err != nil {
		t.Fatalf("GetPeriodUsage: %v", err)
	}

	// A second pass with nothing new: the count is identical, the stamp is not.
	if _, err := f.svc.RefreshPeriodUsage(ctx, f.orgID, start, end); err != nil {
		t.Fatalf("RefreshPeriodUsage (second): %v", err)
	}
	second, err := f.svc.GetPeriodUsage(ctx, f.orgID, start)
	if err != nil {
		t.Fatalf("GetPeriodUsage (second): %v", err)
	}
	if second.EventCount != first.EventCount {
		t.Errorf("event_count moved from %d to %d with no new events", first.EventCount, second.EventCount)
	}
	if !second.UsageComputedAt.After(first.UsageComputedAt) {
		t.Errorf("usage_computed_at did not advance (%s → %s); an idle org is now indistinguishable from a dead meter",
			first.UsageComputedAt, second.UsageComputedAt)
	}
}

// An org the meter has genuinely never reached has no stamp to fall back to.
func TestNeverMeteredOrgHasNoStamp(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	start, _ := coreusage.CalendarMonth(time.Now())

	usage, err := f.svc.GetPeriodUsage(t.Context(), f.orgID, start)
	if err != nil {
		t.Fatalf("GetPeriodUsage: %v", err)
	}
	if usage.EventCount != 0 || !usage.UsageComputedAt.IsZero() {
		t.Errorf("got %+v, want a zero-valued reading", usage)
	}
}

// pgx reports batch failures through a per-item callback, so a swallowed one
// would let the pass stamp usage_computed_at over counts it never wrote.
func TestRecordDailyUsageSurfacesAFailedBatch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := f.svc.RecordDailyUsage(ctx, []coreusage.DailyUsage{
		{Day: coreusage.FloorDayUTC(time.Now().UTC()), EventCount: 1, ProjectID: f.projectID},
	})
	if err == nil {
		t.Fatal("RecordDailyUsage returned nil for a batch that could not run")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
	if !strings.Contains(err.Error(), "1 of 1 cells") {
		t.Errorf("err = %q, want it to report how many cells failed", err)
	}
}

// The server builds the service without ClickHouse, so metering from it has to
// fail loudly rather than report an empty window.
func TestMeterWindowWithoutClickHouse(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	start, end := coreusage.CalendarMonth(time.Now().UTC())

	usage, err := f.svc.MeterWindow(t.Context(), start, end)
	if !errors.Is(err, coreusage.ErrNoMeteringConn) {
		t.Errorf("err = %v, want ErrNoMeteringConn", err)
	}
	if usage != nil {
		t.Errorf("usage = %+v, want nil", usage)
	}
}

// Each of these answers with a zero or an empty slice on success, so a swallowed
// error is indistinguishable from "nothing to report" — the pass would go on to
// stamp usage_computed_at over counts it never wrote.
func TestUsageCallsSurfaceFailuresRatherThanEmptyAnswers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ch := testutil.SetupClickHouse(t)
	svc := f.svc.WithClickHouse(ch.Conn)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	now := time.Now().UTC()
	start, end := coreusage.CalendarMonth(now)

	calls := map[string]func() error{
		"MeterWindow": func() error { _, err := svc.MeterWindow(ctx, start, end); return err },
		"OrgPeriods":  func() error { _, err := svc.OrgPeriods(ctx, now); return err },
		// Non-empty on purpose: nil usage short-circuits on the fail-closed guard
		// before any I/O, so it would prove only that the guard exists.
		"DeleteUnmeteredDays": func() error {
			_, err := svc.DeleteUnmeteredDays(ctx,
				[]coreusage.DailyUsage{{Day: coreusage.FloorDayUTC(now), EventCount: 1, ProjectID: f.projectID}},
				start, end)
			return err
		},
		"RefreshPeriodUsage": func() error { _, err := svc.RefreshPeriodUsage(ctx, f.orgID, start, end); return err },
		"PruneUsage":         func() error { _, err := svc.PruneUsage(ctx, now); return err },
		"GetPeriodUsage":     func() error { _, err := svc.GetPeriodUsage(ctx, f.orgID, start); return err },
		"ListDailyUsage":     func() error { _, err := svc.ListDailyUsage(ctx, f.orgID, start, end); return err },
		"CountKnownProjects": func() error { _, err := svc.CountKnownProjects(ctx, []string{f.projectID}); return err },
		"CountStoredDays":    func() error { _, err := svc.CountStoredDays(ctx, start, end); return err },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil {
				t.Error("returned nil against a cancelled context")
			}
		})
	}
}

// Both halves matter. A prune that deletes nothing leaves the table growing
// forever; a prune that deletes too much silently destroys the counts the whole
// feature exists to report, and "pruned == 1" alone cannot tell the two apart —
// `where day < $1 or true` satisfies it just as well when the only row seeded is
// already past the boundary.
func TestPruneUsageDropsOldDaysAndKeepsRecentOnes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ctx := t.Context()

	now := time.Now().UTC()
	old := coreusage.FloorDayUTC(now.AddDate(0, 0, -500))
	recent := coreusage.FloorDayUTC(now.AddDate(0, 0, -10))
	if err := f.svc.RecordDailyUsage(ctx, []coreusage.DailyUsage{
		{Day: old, EventCount: 7, ProjectID: f.projectID},
		{Day: recent, EventCount: 11, ProjectID: f.projectID},
	}); err != nil {
		t.Fatalf("RecordDailyUsage: %v", err)
	}

	pruned, err := f.svc.PruneUsage(ctx, now.AddDate(0, 0, -400))
	if err != nil {
		t.Fatalf("PruneUsage: %v", err)
	}
	if pruned != 1 {
		t.Errorf("pruned = %d, want 1 (only the row past the boundary)", pruned)
	}

	kept, err := f.svc.ListDailyUsage(ctx, f.orgID, recent, now)
	if err != nil {
		t.Fatalf("ListDailyUsage: %v", err)
	}
	if len(kept) != 1 {
		t.Fatalf("in-retention rows = %d, want 1 — the prune took a day it should have kept", len(kept))
	}
	if !kept[0].Day.Equal(recent) || kept[0].EventCount != 11 {
		t.Errorf("kept %s = %d, want %s = 11", kept[0].Day, kept[0].EventCount, recent)
	}
}

// RecordDailyUsage batches in chunks of 1000, and every other test here uses a
// handful of cells — so the chunk-boundary slice and the loop that advances past
// it have never run. A deployment with a few thousand projects hits both on its
// first full recompute.
func TestRecordDailyUsageSpansMultipleBatchChunks(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ctx := t.Context()

	// Past one chunk and into the next, with an uneven remainder so the final
	// partial chunk is exercised too.
	const cells = 1001
	start := coreusage.FloorDayUTC(time.Now().UTC().AddDate(0, 0, -cells))
	usage := make([]coreusage.DailyUsage, 0, cells)
	for i := range cells {
		usage = append(usage, coreusage.DailyUsage{
			Day:        start.AddDate(0, 0, i),
			EventCount: int64(i + 1),
			ProjectID:  f.projectID,
		})
	}

	if err := f.svc.RecordDailyUsage(ctx, usage); err != nil {
		t.Fatalf("RecordDailyUsage: %v", err)
	}

	// Read back the two cells that sit either side of the chunk boundary, plus the
	// last one, so a loop that drops a chunk cannot pass.
	for _, want := range []struct {
		offset int
		count  int64
	}{{999, 1000}, {1000, 1001}, {cells - 1, cells}} {
		day := start.AddDate(0, 0, want.offset)
		got, err := f.svc.ListDailyUsage(ctx, f.orgID, day, day.AddDate(0, 0, 1))
		if err != nil {
			t.Fatalf("ListDailyUsage(%s): %v", day, err)
		}
		if len(got) != 1 || got[0].EventCount != want.count {
			t.Errorf("cell at offset %d = %v, want one row counting %d", want.offset, got, want.count)
		}
	}
}

// The retention window is a stated number, not an implementation detail: shrink
// it and a deployment silently loses history it is meant to keep.
func TestPruneUsageHonorsTheRetentionBoundary(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ctx := t.Context()

	now := time.Now().UTC()
	// One day either side of the 390-day retention boundary the cron job uses.
	inside := coreusage.FloorDayUTC(now.AddDate(0, 0, -389))
	outside := coreusage.FloorDayUTC(now.AddDate(0, 0, -391))
	if err := f.svc.RecordDailyUsage(ctx, []coreusage.DailyUsage{
		{Day: inside, EventCount: 1, ProjectID: f.projectID},
		{Day: outside, EventCount: 2, ProjectID: f.projectID},
	}); err != nil {
		t.Fatalf("RecordDailyUsage: %v", err)
	}

	pruned, err := f.svc.PruneUsage(ctx, now.AddDate(0, 0, -390))
	if err != nil {
		t.Fatalf("PruneUsage: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("pruned = %d, want exactly the day outside the window", pruned)
	}

	kept, err := f.svc.ListDailyUsage(ctx, f.orgID, inside, now)
	if err != nil {
		t.Fatalf("ListDailyUsage: %v", err)
	}
	if len(kept) != 1 || !kept[0].Day.Equal(inside) {
		t.Errorf("kept %v, want the day one inside the boundary (%s)", kept, inside)
	}
}

// Counts have to converge downward, not only upward. A GDPR erasure that removes
// one user's events from a day the others still occupy leaves that day with a
// smaller count, and an upsert gated on "greater than" would hold the old number
// forever while usage_computed_at kept advancing over it.
func TestRecordDailyUsageConvergesDownward(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ctx := t.Context()
	day := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)

	for _, count := range []int64{10, 7} {
		if err := f.svc.RecordDailyUsage(ctx, []coreusage.DailyUsage{
			{Day: day, EventCount: count, ProjectID: f.projectID},
		}); err != nil {
			t.Fatalf("RecordDailyUsage(%d): %v", count, err)
		}
	}

	daily, err := f.svc.ListDailyUsage(ctx, f.orgID, day, day.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("ListDailyUsage: %v", err)
	}
	if len(daily) != 1 {
		t.Fatalf("got %d cells, want 1", len(daily))
	}
	if daily[0].EventCount != 7 {
		t.Errorf("event_count = %d, want 7: the day never converged downward", daily[0].EventCount)
	}
}

// The drop takes "everything the pass saw" as its argument, so an empty slice has
// to mean "drop nothing" rather than "nothing was seen, so drop it all" -- with no
// cells to keep, every stored row in the window matches as unmetered.
func TestDeleteUnmeteredDaysFailsClosedOnAnEmptyRead(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	ctx := t.Context()
	day := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	if err := f.svc.RecordDailyUsage(ctx, []coreusage.DailyUsage{
		{Day: day, EventCount: 4, ProjectID: f.projectID},
	}); err != nil {
		t.Fatalf("RecordDailyUsage: %v", err)
	}

	dropped, err := f.svc.DeleteUnmeteredDays(ctx, nil, day, day.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("DeleteUnmeteredDays: %v", err)
	}
	if dropped != 0 {
		t.Errorf("dropped %d rows on an empty read, want 0", dropped)
	}

	stored, err := f.svc.CountStoredDays(ctx, day, day.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("CountStoredDays: %v", err)
	}
	if stored != 1 {
		t.Errorf("%d cells survived the empty read, want 1", stored)
	}
}
