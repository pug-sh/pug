package retention_test

import (
	"context"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/xid"

	"github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/core/billing/entitlement"
	"github.com/pug-sh/pug/internal/core/retention"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

const month = 30 * 24 * time.Hour

var (
	t0 = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	// A 90-day length run 30 days after t0 cuts before 2026-08-01.
	old    = time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	recent = time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
)

func TestPassCutsPastTheLengthAfterTheWait(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	org, other := f.org(t, 90), f.org(t, 0)
	p, q := f.project(t, org), f.project(t, other)
	// A session and a person on both sides of the cut stay whole.
	gone, straddling := uuid.NewString(), uuid.NewString()
	f.event(t, p, "gone", "gone", gone, old)
	f.event(t, p, "both", "both", straddling, old)
	f.event(t, p, "both", "both", straddling, recent)
	f.event(t, q, "kept", "kept", uuid.NewString(), old)
	f.bucket(t, p, old)
	f.bucket(t, p, recent)
	f.bucket(t, q, old)
	kept := f.counts(t, q)

	// The switch off still moves the state on, so turning it on starts no wait.
	dry := f.service(t, billing.Config{}, false)
	pass(t, dry, t0)
	if got, want := f.state(t, org), (row{pendingDays: 90, pendingSince: t0}); !got.equal(want) {
		t.Fatalf("state after the first pass = %+v, want %+v", got, want)
	}
	pass(t, dry, t0.Add(month))
	if got := f.state(t, org); !got.equal(row{days: 90}) {
		t.Fatalf("state after the wait = %+v, want 90 days", got)
	}
	if n := deletes(t, f, p); n != 0 {
		t.Fatalf("the switch off queued %d deletes", n)
	}
	if got := f.counts(t, p); got["events"] != 3 {
		t.Fatalf("the switch off deleted events: %v", got)
	}

	merged := []struct{ table, query, gone string }{
		{"dashboard_session_rollup",
			"SELECT toString(session_id), countMerge(event_count_state) FROM dashboard_session_rollup WHERE project_id = ? GROUP BY session_id",
			gone},
		{"distinct_id_activity_states",
			"SELECT distinct_id, countMerge(total_events_state) FROM distinct_id_activity_states WHERE project_id = ? GROUP BY distinct_id",
			"gone"},
		{"event_names",
			"SELECT kind, countMerge(event_count) FROM event_names WHERE project_id = ? GROUP BY kind",
			"gone"},
	}
	want := map[string]map[string]uint64{}
	for _, m := range merged {
		want[m.table] = f.keyCounts(t, m.query, p)
		if _, ok := want[m.table][m.gone]; !ok || len(want[m.table]) != 2 {
			t.Fatalf("%s before the cut = %v, want the gone key and the straddling one", m.table, want[m.table])
		}
		delete(want[m.table], m.gone)
	}

	pass(t, f.service(t, billing.Config{}, true), t0.Add(month+time.Hour))
	waitForMutations(t, f)

	if got := f.count(t, "SELECT count() FROM events WHERE project_id = ?", p); got != 1 {
		t.Errorf("events left = %d, want the 1 after the cut", got)
	}
	if got := f.count(t, "SELECT count() FROM dashboard_event_rollup_daily WHERE project_id = ? AND day < '2026-08-01'", p); got != 0 {
		t.Errorf("rollup rows before the cut = %d, want 0", got)
	}
	if got := f.count(t, "SELECT count() FROM dashboard_event_rollup_daily WHERE project_id = ? AND day = '2026-08-05'", p); got == 0 {
		t.Error("the rollup lost the day after the cut")
	}
	if got := f.count(t, "SELECT count() FROM property_keys_event_buckets WHERE project_id = ?", p); got != 1 {
		t.Errorf("buckets left = %d, want the 1 after the cut", got)
	}
	for _, m := range merged {
		if got := f.keyCounts(t, m.query, p); !maps.Equal(got, want[m.table]) {
			t.Errorf("%s = %v, want %v: the straddling key whole, the gone one deleted", m.table, got, want[m.table])
		}
	}
	if got := f.counts(t, q); !maps.Equal(got, kept) {
		t.Errorf("an org with no length lost rows: %v, want %v", got, kept)
	}
}

// Any unfinished delete holds a table to a later run, so retention's never pile up.
func TestPassSkipsATableWithADeleteUnfinished(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	org := f.org(t, 90)
	f.seedState(t, org, 90)
	p := f.project(t, org)
	f.event(t, p, "gone", "gone", uuid.NewString(), old)

	// Mutations run in the merge pool, so stopping merges holds one open.
	f.exec(t, "SYSTEM STOP MERGES events")
	t.Cleanup(func() { _ = f.ch.Conn.Exec(context.Background(), "SYSTEM START MERGES events") })
	f.exec(t, "ALTER TABLE events DELETE WHERE project_id = 'elsewhere' SETTINGS mutations_sync = 0")

	pass(t, f.service(t, billing.Config{}, true), t0.Add(month))
	got := f.keyCounts(t,
		"SELECT table, count() FROM system.mutations WHERE database = currentDatabase() AND position(command, ?) > 0 GROUP BY table", p)
	if got["events"] != 0 {
		t.Errorf("queued %d deletes on events beside an unfinished one", got["events"])
	}
	if got["dashboard_event_rollup_daily"] != 1 {
		t.Errorf("deletes on the rollup = %d, want 1: one busy table held up the rest", got["dashboard_event_rollup_daily"])
	}
}

// Deletes follow each org's enforced length, never one still waiting.
func TestPassDeletesOnlyOnTheEnforcedLength(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	due, longer, waiting, held := f.org(t, 90), f.org(t, 365), f.org(t, 90), f.org(t, 0)
	f.seedState(t, due, 90)
	f.seedState(t, longer, 365)
	f.seedState(t, held, 1825)
	f.subscription(t, held, "cancelled")
	ancient := time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC)
	projects := map[string]string{}
	for _, org := range []string{due, longer, waiting, held} {
		projects[org] = f.project(t, org)
		for _, at := range []time.Time{ancient, old, recent} {
			f.event(t, projects[org], "user", "page_view", uuid.NewString(), at)
		}
	}

	pass(t, f.service(t, billing.Config{Enabled: true}, true), t0.Add(month))
	waitForMutations(t, f)
	for _, tc := range []struct {
		name string
		org  string
		want uint64
	}{
		{"90 days cuts before August", due, 1},
		{"a year cuts before November 2025", longer, 2},
		{"a new shorter length waits", waiting, 3},
		{"a lapsed org keeps its paid length until an expire", held, 3},
	} {
		if got := f.count(t, "SELECT count() FROM events WHERE project_id = ?", projects[tc.org]); got != tc.want {
			t.Errorf("%s: events left = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// ClickHouse retries a failing delete forever, so it fails each pass until killed.
func TestPassFailsOnAFailingDelete(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	org := f.org(t, 90)
	f.seedState(t, org, 90)
	f.event(t, f.project(t, org), "gone", "gone", uuid.NewString(), old)

	// Accepted, then fails on every part: kind is not a number.
	f.exec(t, "ALTER TABLE events DELETE WHERE toUInt32(kind) = 1 SETTINGS mutations_sync = 0")
	t.Cleanup(func() {
		_ = f.ch.Conn.Exec(context.Background(), "KILL MUTATION WHERE database = currentDatabase() AND table = 'events'")
	})
	deadline := time.Now().Add(30 * time.Second)
	for f.count(t, "SELECT count() FROM system.mutations WHERE database = currentDatabase() AND latest_fail_reason != ''") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the delete never failed")
		}
		time.Sleep(50 * time.Millisecond)
	}

	if err := f.service(t, billing.Config{}, true).Pass(t.Context(), t0.Add(month)); !errors.Is(err, retention.ErrDeleteFailing) {
		t.Errorf("Pass = %v, want ErrDeleteFailing", err)
	}
}

// An org that stops paying keeps its paid length until an operator expires it.
func TestPassHoldsAPaidLengthForAnExpire(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	f := newFixture(t)
	svc := f.service(t, billing.Config{Enabled: true}, false)
	ctx := t.Context()
	lapsed, free, paying, declined := f.org(t, 0), f.org(t, 0), f.org(t, 0), f.org(t, 0)
	f.seedState(t, lapsed, 1825)
	f.subscription(t, lapsed, "cancelled")
	f.subscription(t, paying, "active")
	f.subscription(t, declined, "failed")
	const freeDays = 365

	pass(t, svc, t0)
	for _, tc := range []struct {
		org  string
		want row
	}{
		{lapsed, row{days: 1825, pendingDays: freeDays}},
		{free, row{pendingDays: freeDays, pendingSince: t0}},
		{declined, row{pendingDays: freeDays, pendingSince: t0}},
		{paying, row{pendingDays: 1825, pendingSince: t0}},
	} {
		if got := f.state(t, tc.org); !got.equal(tc.want) {
			t.Errorf("state = %+v, want %+v", got, tc.want)
		}
	}

	if _, err := svc.Expire(ctx, paying, "ops/1", t0); !errors.Is(err, retention.ErrOrgPays) {
		t.Errorf("Expire(paying) = %v, want ErrOrgPays", err)
	}
	if _, err := svc.Expire(ctx, free, "ops/1", t0); !errors.Is(err, retention.ErrNothingWaiting) {
		t.Errorf("Expire(free) = %v, want ErrNothingWaiting", err)
	}
	if _, err := svc.Expire(ctx, lapsed, " ", t0); !errors.Is(err, retention.ErrActorRequired) {
		t.Errorf("Expire with a blank actor = %v, want ErrActorRequired", err)
	}

	t1 := t0.Add(2 * month)
	pass(t, svc, t1)
	if got := f.state(t, lapsed); got.days != 1825 {
		t.Errorf("the lapsed org's length = %d before an expire, want 1825", got.days)
	}
	if got := f.state(t, free); !got.equal(row{days: freeDays}) {
		t.Errorf("the free org = %+v after its wait, want %d days", got, freeDays)
	}

	got, err := svc.Expire(ctx, lapsed, "ops/1", t1)
	if err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if want := (retention.Expired{Days: 1825, PendingDays: freeDays, AppliesAt: t1.Add(month)}); got != want {
		t.Errorf("Expire = %+v, want %+v", got, want)
	}
	if _, err := svc.Expire(ctx, lapsed, "ops/2", t1); !errors.Is(err, retention.ErrNothingWaiting) {
		t.Errorf("a second Expire = %v, want ErrNothingWaiting", err)
	}
	pass(t, svc, t1.Add(month-time.Second))
	if got, want := f.state(t, lapsed), (row{days: 1825, pendingDays: freeDays, pendingSince: t1, expiredBy: "ops/1"}); !got.equal(want) {
		t.Errorf("state during the wait = %+v, want %+v", got, want)
	}
	pass(t, svc, t1.Add(month))
	if got := f.state(t, lapsed); !got.equal(row{days: freeDays}) {
		t.Errorf("state after the wait = %+v, want %d days", got, freeDays)
	}
}

type fixture struct {
	pg *testutil.TestPostgres
	ch *testutil.TestClickHouse
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return &fixture{pg: testutil.SetupPostgres(t), ch: testutil.SetupClickHouse(t)}
}

func (f *fixture) service(t *testing.T, cfg billing.Config, enabled bool) *retention.Service {
	t.Helper()
	ents, err := entitlement.NewService(f.pg.PgW, f.pg.PgW, cfg)
	if err != nil {
		t.Fatalf("entitlement.NewService: %v", err)
	}
	return retention.NewService(f.pg.PgW, f.ch.Conn, ents, retention.Config{Enabled: enabled})
}

func pass(t *testing.T, svc *retention.Service, now time.Time) {
	t.Helper()
	if err := svc.Pass(t.Context(), now); err != nil {
		t.Fatalf("Pass(%s): %v", now.Format(time.RFC3339), err)
	}
}

// org creates an org with an override of days, or none for 0.
func (f *fixture) org(t *testing.T, days int64) string {
	t.Helper()
	ctx := t.Context()
	orgID := xid.New().String()
	if _, err := dbwrite.New(f.pg.PgW).CreateOrg(ctx, dbwrite.CreateOrgParams{ID: orgID, DisplayName: "Org"}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if days == 0 {
		return orgID
	}
	ents, err := entitlement.NewService(f.pg.PgW, f.pg.PgW, billing.Config{})
	if err != nil {
		t.Fatalf("entitlement.NewService: %v", err)
	}
	change := entitlement.Change{PlanSlug: entitlement.SlugFree, RetentionDays: &days}
	if _, err := ents.SetPlan(ctx, orgID, "ops/test", change); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}
	return orgID
}

func (f *fixture) project(t *testing.T, orgID string) string {
	t.Helper()
	id := xid.New().String()
	if _, err := dbwrite.New(f.pg.PgW).CreateProject(t.Context(),
		dbwrite.CreateProjectParams{ID: id, OrgID: orgID, DisplayName: id}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	return id
}

func (f *fixture) seedState(t *testing.T, orgID string, days int64) {
	t.Helper()
	if _, err := f.pg.PgW.Exec(t.Context(),
		"insert into retention_state (days, org_id) values ($1, $2)", days, orgID); err != nil {
		t.Fatalf("seed retention state: %v", err)
	}
}

func (f *fixture) subscription(t *testing.T, orgID, status string) {
	t.Helper()
	if _, err := f.pg.PgW.Exec(t.Context(),
		`insert into billing_subscriptions (currency, id, org_id, plan_slug, price_cents, provider,
		   provider_customer_id, provider_status, provider_sub_id, provider_updated_at, status)
		 values ('USD', $1, $2, $3, 100, 'dodo', 'cus_1', $4, $5, now(), $4)`,
		xid.New().String(), orgID, entitlement.SlugUsage, status, "sub_"+orgID); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}
}

type row struct {
	days, pendingDays int64
	pendingSince      time.Time
	expiredBy         string
}

func (r row) equal(o row) bool {
	return r.days == o.days && r.pendingDays == o.pendingDays &&
		r.pendingSince.Equal(o.pendingSince) && r.expiredBy == o.expiredBy
}

func (f *fixture) state(t *testing.T, orgID string) row {
	t.Helper()
	var r row
	var since *time.Time
	if err := f.pg.PgW.QueryRow(t.Context(),
		`select coalesce(days, 0), coalesce(pending_days, 0), pending_since, coalesce(expired_by, '')
		 from retention_state where org_id = $1`, orgID,
	).Scan(&r.days, &r.pendingDays, &since, &r.expiredBy); err != nil {
		t.Fatalf("read retention state: %v", err)
	}
	if since != nil {
		r.pendingSince = *since
	}
	return r
}

func (f *fixture) event(t *testing.T, projectID, distinctID, kind, sessionID string, at time.Time) {
	t.Helper()
	testutil.InsertEvent(t.Context(), t, f.ch.Conn, uuid.NewString(), projectID, distinctID, kind, sessionID,
		map[string]string{"$pathname": "/"}, nil, at)
}

// bucket stands in for the refreshable view, which only reads recent events.
func (f *fixture) bucket(t *testing.T, projectID string, at time.Time) {
	t.Helper()
	f.exec(t, `INSERT INTO property_keys_event_buckets
		(project_id, map_type, kind, bucket_time, key, value_type, event_count, last_seen)
		VALUES (?, 'auto', 'page_view', ?, '$pathname', 'String', 1, ?)`, projectID, at, at)
}

func (f *fixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if err := f.ch.Conn.Exec(t.Context(), query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func (f *fixture) count(t *testing.T, query string, args ...any) uint64 {
	t.Helper()
	var n uint64
	if err := f.ch.Conn.QueryRow(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// keyCounts reads a query of (key, count) rows.
func (f *fixture) keyCounts(t *testing.T, query string, args ...any) map[string]uint64 {
	t.Helper()
	rows, err := f.ch.Conn.Query(t.Context(), query, args...)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]uint64{}
	for rows.Next() {
		var key string
		var n uint64
		if err := rows.Scan(&key, &n); err != nil {
			t.Fatalf("scan %s: %v", query, err)
		}
		out[key] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return out
}

// counts is the project's rows in each table retention cuts.
func (f *fixture) counts(t *testing.T, projectID string) map[string]uint64 {
	t.Helper()
	out := map[string]uint64{}
	for _, table := range []string{"events", "dashboard_event_rollup_daily", "property_keys_event_buckets",
		"dashboard_session_rollup", "distinct_id_activity_states", "event_names"} {
		out[table] = f.count(t, "SELECT count() FROM "+table+" WHERE project_id = ?", projectID)
	}
	return out
}

// deletes counts the mutations naming the project; the migrations leave their own.
func deletes(t *testing.T, f *fixture, projectID string) uint64 {
	t.Helper()
	return f.count(t,
		"SELECT count() FROM system.mutations WHERE database = currentDatabase() AND position(command, ?) > 0", projectID)
}

func waitForMutations(t *testing.T, f *fixture) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for f.count(t, "SELECT count() FROM system.mutations WHERE database = currentDatabase() AND is_done = 0") > 0 {
		if time.Now().After(deadline) {
			t.Fatal("deletes still open after 30s")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
