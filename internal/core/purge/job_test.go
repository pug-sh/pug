package purge_test

import (
	"crypto/rand"
	"encoding/hex"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/xid"

	"github.com/pug-sh/pug/internal/core/projects"
	"github.com/pug-sh/pug/internal/core/purge"
	coreusage "github.com/pug-sh/pug/internal/core/usage"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

// Every table holding a project's rows must go with its projects row, or the
// last step leaves it behind. Usage and the ledger are kept on purpose.
func TestEveryProjectTableCascades(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	ctx := t.Context()
	kept := map[string]bool{"usage_daily": true, "project_deletions": true}

	cascades := map[string]bool{}
	rows, err := pg.PgRO.Query(ctx, `
		select conrelid::regclass::text, confdeltype = 'c'
		from pg_constraint
		where contype = 'f' and confrelid = 'projects'::regclass`)
	if err != nil {
		t.Fatalf("list foreign keys: %v", err)
	}
	for rows.Next() {
		var table string
		var cascade bool
		if err := rows.Scan(&table, &cascade); err != nil {
			t.Fatalf("scan foreign key: %v", err)
		}
		if !cascade {
			t.Errorf("%s references projects without on delete cascade, so the last step fails", table)
		}
		cascades[table] = cascade
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list foreign keys: %v", err)
	}

	for _, table := range pgProjectTables(t, pg) {
		switch {
		case kept[table] && cascades[table]:
			t.Errorf("%s cascades from projects, so the last step deletes what it must keep", table)
		case !kept[table] && !cascades[table]:
			t.Errorf("%s has a project_id but no cascade from projects, so a deleted project's rows stay", table)
		}
	}
}

func TestPassErasesADeletedProject(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	doomed := f.seedProject(t, "Doomed")
	kept := f.seedProject(t, "Kept")
	refreshProfileKeys(t, f.ch)
	keptPG, keptCH := pgCounts(t, f.pg, kept), chCounts(t, f.ch, kept)

	f.delete(t, doomed, requestedAt)
	f.pass(t, requestedAt.Add(30*time.Minute))
	if d := f.deletion(t, doomed); d.Status != "pending" {
		t.Fatalf("within the hour: status %q, want pending", d.Status)
	}

	start := requestedAt.Add(time.Hour)
	f.pass(t, start)
	if d := f.deletion(t, doomed); d.Status != "deleting" || d.Rounds != 1 {
		t.Fatalf("after the hour: status %q, rounds %d; want deleting, 1", d.Status, d.Rounds)
	}
	waitForDeletes(t, f.ch)
	refreshProfileKeys(t, f.ch)
	f.pass(t, start.Add(5*time.Minute))

	d := f.deletion(t, doomed)
	if d.Status != "done" || d.Error != "" {
		t.Fatalf("status %q, error %q; want done with no error", d.Status, d.Error)
	}
	for table, n := range pgCounts(t, f.pg, doomed) {
		if n != 0 && table != "usage_daily" && table != "project_deletions" {
			t.Errorf("postgres %s still holds %d of the deleted project's rows", table, n)
		}
	}
	for table, n := range chCounts(t, f.ch, doomed) {
		if n != 0 {
			t.Errorf("clickhouse %s still holds %d of the deleted project's rows", table, n)
		}
	}
	if got := pgCounts(t, f.pg, kept); !maps.Equal(got, keptPG) {
		t.Errorf("kept project's postgres rows = %v, want %v", got, keptPG)
	}
	if got := chCounts(t, f.ch, kept); !maps.Equal(got, keptCH) {
		t.Errorf("kept project's clickhouse rows = %v, want %v", got, keptCH)
	}
}

func TestRowsWrittenMidDeleteGetAnotherRound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	doomed := f.seedProject(t, "Doomed")
	f.delete(t, doomed, requestedAt)
	start := requestedAt.Add(time.Hour)
	f.pass(t, start)
	waitForDeletes(t, f.ch)

	// A message queued before the delete lands after it.
	insertEvent(t, f.ch, doomed, eventDay)
	refreshProfileKeys(t, f.ch)
	f.pass(t, start.Add(5*time.Minute))
	if d := f.deletion(t, doomed); d.Status != "deleting" || d.Rounds != 1 {
		t.Fatalf("inside the round's hour: status %q, rounds %d; want deleting, 1", d.Status, d.Rounds)
	}

	f.pass(t, start.Add(time.Hour))
	d := f.deletion(t, doomed)
	if d.Rounds != 2 || !strings.Contains(d.Error, "rows came back") {
		t.Fatalf("an hour on: rounds %d, error %q; want a second round, recorded", d.Rounds, d.Error)
	}
	waitForDeletes(t, f.ch)
	f.pass(t, start.Add(65*time.Minute))
	if d := f.deletion(t, doomed); d.Status != "done" || d.Error == "" {
		t.Errorf("status %q, error %q; want done with the round's error kept", d.Status, d.Error)
	}
	if n := rowCount(t, f.ch, "events", doomed); n != 0 {
		t.Errorf("events holds %d of the deleted project's rows", n)
	}
}

func TestWatchReopensADoneDeletion(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	doomed := f.seedProject(t, "Doomed")
	f.delete(t, doomed, requestedAt)
	start := requestedAt.Add(time.Hour)
	f.pass(t, start)
	waitForDeletes(t, f.ch)
	refreshProfileKeys(t, f.ch)
	doneAt := start.Add(5 * time.Minute)
	f.pass(t, doneAt)
	if d := f.deletion(t, doomed); d.Status != "done" {
		t.Fatalf("status %q, want done", d.Status)
	}

	insertEvent(t, f.ch, doomed, eventDay)
	f.pass(t, doneAt.Add(2*time.Hour))
	if d := f.deletion(t, doomed); d.Status != "deleting" || d.Rounds != 2 {
		t.Fatalf("rows came back: status %q, rounds %d; want deleting, 2", d.Status, d.Rounds)
	}
	waitForDeletes(t, f.ch)
	f.pass(t, doneAt.Add(3*time.Hour))
	if d := f.deletion(t, doomed); d.Status != "done" {
		t.Fatalf("status %q, want done again", d.Status)
	}

	// Past the watch, nothing looks.
	insertEvent(t, f.ch, doomed, eventDay)
	f.pass(t, doneAt.Add(31*24*time.Hour))
	if d := f.deletion(t, doomed); d.Status != "done" {
		t.Errorf("31 days on: status %q, want done", d.Status)
	}
}

func TestPassFreezesUsageOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	doomed := f.seedProject(t, "Doomed")
	insertEvent(t, f.ch, doomed, eventDay.Add(time.Hour))
	insertEvent(t, f.ch, doomed, eventDay.Add(-23*time.Hour))
	f.delete(t, doomed, requestedAt)

	start := requestedAt.Add(time.Hour)
	f.pass(t, start)
	waitForDeletes(t, f.ch)
	refreshProfileKeys(t, f.ch)
	f.pass(t, start.Add(5*time.Minute))
	if d := f.deletion(t, doomed); d.Status != "done" {
		t.Fatalf("status %q, want done", d.Status)
	}

	// The seed stored 1 for eventDay; the pass counted 2 there and 1 the day before.
	want := map[time.Time]int64{eventDay: 2, eventDay.AddDate(0, 0, -1): 1}
	if got := f.usageDays(t, doomed); !maps.Equal(got, want) {
		t.Fatalf("frozen days = %v, want %v", got, want)
	}

	// Reopened, its late rows are erased but not counted.
	insertEvent(t, f.ch, doomed, eventDay)
	f.pass(t, start.Add(3*time.Hour))
	waitForDeletes(t, f.ch)
	f.pass(t, start.Add(4*time.Hour))
	if d := f.deletion(t, doomed); d.Status != "done" || d.Rounds != 2 {
		t.Fatalf("status %q, rounds %d; want done after a second round", d.Status, d.Rounds)
	}
	if got := f.usageDays(t, doomed); !maps.Equal(got, want) {
		t.Errorf("after the reopen: days = %v, want %v", got, want)
	}

	svc := coreusage.NewService(f.pg.PgRO, f.pg.PgW)
	periodStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	total, err := svc.RefreshPeriodUsage(t.Context(), f.orgID, periodStart, periodStart.AddDate(0, 1, 0))
	if err != nil {
		t.Fatalf("RefreshPeriodUsage: %v", err)
	}
	if total != 3 {
		t.Errorf("org period total = %d, want the 3 frozen events", total)
	}
}

func TestPassFlagsADeletionStuckForADay(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	doomed := f.seedProject(t, "Doomed")
	f.delete(t, doomed, requestedAt)
	start := requestedAt.Add(time.Hour)
	f.pass(t, start)
	waitForDeletes(t, f.ch)

	// property_keys_profile_current is never refreshed, so it never empties.
	f.pass(t, start.Add(23*time.Hour))
	if d := f.deletion(t, doomed); d.Error != "" {
		t.Fatalf("under a day: error %q, want none", d.Error)
	}
	f.pass(t, start.Add(24*time.Hour))
	if d := f.deletion(t, doomed); d.Status != "deleting" || !strings.Contains(d.Error, "not done") {
		t.Errorf("a day on: status %q, error %q; want deleting with an error", d.Status, d.Error)
	}
}

var (
	// The fixture's org is anchored on the 1st, so these sit in June's period.
	requestedAt = time.Date(2026, 6, 20, 10, 0, 0, 0, time.UTC)
	eventDay    = time.Date(2026, 6, 19, 0, 0, 0, 0, time.UTC)
)

type fixture struct {
	pg       *testutil.TestPostgres
	ch       *testutil.TestClickHouse
	svc      *purge.Service
	projects *projects.Service
	orgID    string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pg := testutil.SetupPostgres(t)
	ch := testutil.SetupClickHouse(t)
	rd := testutil.SetupRedis(t)

	orgID := xid.New().String()
	if _, err := dbwrite.New(pg.PgW).CreateOrg(t.Context(), dbwrite.CreateOrgParams{ID: orgID, DisplayName: "Org"}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	testutil.SetOrgCreateTime(t, pg.PgW, orgID, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	return &fixture{
		pg:       pg,
		ch:       ch,
		svc:      purge.NewService(pg.PgW, ch.Conn),
		projects: projects.NewService(pg.PgRO, pg.PgW, projects.NewRepo(dbread.New(pg.PgRO), rd.Client)),
		orgID:    orgID,
	}
}

// seedProject creates a project with rows in every Postgres and ClickHouse table
// that holds one, and one counted event on eventDay.
func (f *fixture) seedProject(t *testing.T, name string) string {
	t.Helper()
	ctx := t.Context()
	p, err := f.projects.CreateProject(ctx, f.orgID, name, "")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	profileID, dashboardID := xid.New().String(), xid.New().String()
	token := make([]byte, 32)
	_, _ = rand.Read(token)
	for _, s := range []struct {
		sql  string
		args []any
	}{
		{"insert into profiles (id, project_id, external_id) values ($1, $2, 'user-1')",
			[]any{profileID, p.ID}},
		{"insert into profile_devices (id, platform, profile_id, project_id, token) values ($1, 'web', $2, $3, 'push')",
			[]any{uuid.NewString(), profileID, p.ID}},
		{"insert into dashboards (id, project_id, display_name) values ($1, $2, 'Board')",
			[]any{dashboardID, p.ID}},
		{"insert into dashboard_tiles (id, dashboard_id, kind, markdown_body) values ($1, $2, 2, 'Hi')",
			[]any{xid.New().String(), dashboardID}},
		{"insert into dashboard_shares (id, dashboard_id, project_id, share_token) values ($1, $2, $3, $4)",
			[]any{xid.New().String(), dashboardID, p.ID, hex.EncodeToString(token)}},
		{"insert into campaigns (id, name, notification_data, project_id) values ($1, 'Push', '{}', $2)",
			[]any{xid.New().String(), p.ID}},
		{"insert into compliance_requests (id, project_id, kind, external_id) values ($1, $2, 'erase', 'user-1')",
			[]any{xid.New().String(), p.ID}},
		{"insert into usage_daily (day, event_count, org_id, project_id) values ($1, 1, $2, $3)",
			[]any{eventDay, f.orgID, p.ID}},
	} {
		if _, err := f.pg.PgW.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed %q: %v", s.sql, err)
		}
	}
	seedClickHouse(t, f.ch, p.ID, eventDay.Add(2*time.Hour))
	return p.ID
}

// delete runs the delete request, dated at.
func (f *fixture) delete(t *testing.T, projectID string, at time.Time) {
	t.Helper()
	if err := f.projects.DeleteProject(t.Context(), f.orgID, projectID, "customer test"); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if _, err := f.pg.PgW.Exec(t.Context(),
		"update project_deletions set requested_at = $1 where project_id = $2", at, projectID); err != nil {
		t.Fatalf("date the deletion: %v", err)
	}
}

func (f *fixture) pass(t *testing.T, now time.Time) {
	t.Helper()
	if err := f.svc.Pass(t.Context(), now); err != nil {
		t.Fatalf("Pass(%s): %v", now.Format(time.RFC3339), err)
	}
}

type deletionRow struct {
	Status string
	Rounds int32
	Error  string
}

func (f *fixture) deletion(t *testing.T, projectID string) deletionRow {
	t.Helper()
	var d deletionRow
	if err := f.pg.PgRO.QueryRow(t.Context(),
		"select status, rounds, error from project_deletions where project_id = $1", projectID,
	).Scan(&d.Status, &d.Rounds, &d.Error); err != nil {
		t.Fatalf("read deletion: %v", err)
	}
	return d
}

func (f *fixture) usageDays(t *testing.T, projectID string) map[time.Time]int64 {
	t.Helper()
	rows, err := f.pg.PgRO.Query(t.Context(),
		"select day, event_count from usage_daily where project_id = $1", projectID)
	if err != nil {
		t.Fatalf("read usage: %v", err)
	}
	defer rows.Close()
	out := map[time.Time]int64{}
	for rows.Next() {
		var day time.Time
		var n int64
		if err := rows.Scan(&day, &n); err != nil {
			t.Fatalf("scan usage: %v", err)
		}
		out[day.UTC()] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read usage: %v", err)
	}
	return out
}

func pgProjectTables(t *testing.T, pg *testutil.TestPostgres) []string {
	t.Helper()
	rows, err := pg.PgRO.Query(t.Context(), `
		select table_name from information_schema.columns
		where table_schema = 'public' and column_name = 'project_id'
		order by table_name`)
	if err != nil {
		t.Fatalf("list project tables: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan project table: %v", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list project tables: %v", err)
	}
	return out
}

func pgCounts(t *testing.T, pg *testutil.TestPostgres, projectID string) map[string]int64 {
	t.Helper()
	queries := map[string]string{
		"projects":        "select count(*) from projects where id = $1",
		"dashboard_tiles": "select count(*) from dashboard_tiles t join dashboards d on d.id = t.dashboard_id where d.project_id = $1",
	}
	for _, table := range pgProjectTables(t, pg) {
		queries[table] = "select count(*) from " + table + " where project_id = $1"
	}
	out := map[string]int64{}
	for table, q := range queries {
		var n int64
		if err := pg.PgRO.QueryRow(t.Context(), q, projectID).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		out[table] = n
	}
	return out
}

func chCounts(t *testing.T, ch *testutil.TestClickHouse, projectID string) map[string]uint64 {
	t.Helper()
	out := map[string]uint64{}
	for _, table := range projectTables(t, ch) {
		out[table] = rowCount(t, ch, table, projectID)
	}
	return out
}

func insertEvent(t *testing.T, ch *testutil.TestClickHouse, projectID string, at time.Time) {
	t.Helper()
	testutil.InsertEvent(t.Context(), t, ch.Conn, uuid.NewString(), projectID, "user-2", "page_view",
		uuid.NewString(), nil, nil, at)
}

// The view rebuilds property_keys_profile_current from profiles every 5 minutes.
func refreshProfileKeys(t *testing.T, ch *testutil.TestClickHouse) {
	t.Helper()
	for _, q := range []string{
		"SYSTEM REFRESH VIEW property_keys_profile_current_mv",
		"SYSTEM WAIT VIEW property_keys_profile_current_mv",
	} {
		if err := ch.Conn.Exec(t.Context(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}
