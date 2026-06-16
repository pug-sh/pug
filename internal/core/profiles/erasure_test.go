package profiles_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pug-sh/pug/internal/core/profiles"
	natsdeps "github.com/pug-sh/pug/internal/deps/nats"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"github.com/rs/xid"
)

// TestErasure_FullEraseReachesEventsAndRollups is the §4.1 acceptance test: a
// data subject's events (across every distinct_id), the per-person ClickHouse
// rollups, the profile (PG + CH), aliases, and devices are all gone after
// erasure, while a control subject and the anonymous dashboard_event_rollup_daily
// aggregate are untouched. It also re-runs the worker to pin idempotency.
func TestErasure_FullEraseReachesEventsAndRollups(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	ch := testutil.SetupClickHouse(t)
	tn := testutil.SetupNATS(t)
	t.Setenv("NATS_URL", tn.URL)

	natsClient, err := natsdeps.New(ctx)
	if err != nil {
		t.Fatalf("create nats client: %v", err)
	}
	defer natsClient.Close()

	projectID := seedProject(t, ctx, pg)
	write := dbwrite.New(pg.PgW)
	now := time.Now().UTC().Truncate(time.Second)

	const (
		externalID = "erase-me@example.com"
		anonID     = "anon-erase-1"
		keepExtID  = "keep@example.com"
	)
	profileID := xid.New().String()
	keepProfileID := xid.New().String()
	s1, s2, s3 := uuid.NewString(), uuid.NewString(), uuid.NewString()

	// PostgreSQL: target profile + an active device, plus a control profile.
	for _, p := range []struct{ id, ext string }{{profileID, externalID}, {keepProfileID, keepExtID}} {
		if _, err := write.UpsertProfileByExternalID(ctx, dbwrite.UpsertProfileByExternalIDParams{
			ID: p.id, ProjectID: projectID, ExternalID: postgres.NewText(p.ext), Properties: map[string]any{},
		}); err != nil {
			t.Fatalf("seed pg profile %s: %v", p.ext, err)
		}
	}
	deviceID := xid.New().String()
	if _, err := write.SaveProfileDevice(ctx, dbwrite.SaveProfileDeviceParams{
		ID: deviceID, Platform: "ios", ProfileID: postgres.NewText(profileID),
		ProjectID: projectID, Properties: map[string]any{}, Status: "active", Token: "tok-" + deviceID,
	}); err != nil {
		t.Fatalf("seed device: %v", err)
	}

	// ClickHouse: target + control profiles, the alias, and events. The target's
	// events are keyed by external_id (session s1) and the anon alias (session s2);
	// the control's by keepExtID (session s3).
	for _, p := range []struct{ id, ext string }{{profileID, externalID}, {keepProfileID, keepExtID}} {
		if err := ch.Conn.Exec(ctx,
			`INSERT INTO profiles (id, project_id, external_id, properties, is_deleted, create_time, update_time) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			p.id, projectID, p.ext, map[string]any{}, uint8(0), now, now,
		); err != nil {
			t.Fatalf("seed ch profile %s: %v", p.ext, err)
		}
	}
	if err := ch.Conn.Exec(ctx,
		`INSERT INTO profile_aliases (alias_id, profile_id, external_id, project_id) VALUES (?, ?, ?, ?)`,
		anonID, profileID, externalID, projectID,
	); err != nil {
		t.Fatalf("seed alias: %v", err)
	}
	for _, e := range []struct {
		distinctID, session string
		n                   int
	}{{externalID, s1, 3}, {anonID, s2, 2}, {keepExtID, s3, 2}} {
		for range e.n {
			testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, e.distinctID, "page_view", e.session,
				map[string]string{}, map[string]string{}, now)
		}
	}

	// The anonymous aggregate must survive erasure (decision "a"). Capture it now.
	rollupBefore := chCount(t, ctx, ch, "SELECT count() FROM dashboard_event_rollup_daily WHERE project_id = ?", projectID)
	if rollupBefore == 0 {
		t.Fatal("dashboard_event_rollup_daily empty after seeding; MV did not populate")
	}

	svc := profiles.NewService(pg.PgW, ch.Conn, natsClient)

	requestID, status, err := svc.RequestErasureByExternalID(ctx, projectID, externalID, "")
	if err != nil {
		t.Fatalf("RequestErasureByExternalID: %v", err)
	}
	if status != profiles.ComplianceStatusPending {
		t.Errorf("status = %q, want pending", status)
	}
	if err := svc.ExecuteErasure(ctx, projectID, requestID); err != nil {
		t.Fatalf("ExecuteErasure: %v", err)
	}

	// Events: the subject's are gone across all distinct_ids; the control survives.
	if got := chCount(t, ctx, ch,
		"SELECT count() FROM events WHERE project_id = ? AND distinct_id IN (?, ?, ?)",
		projectID, externalID, anonID, profileID); got != 0 {
		t.Errorf("erased events remain: %d", got)
	}
	if got := chCount(t, ctx, ch,
		"SELECT count() FROM events WHERE project_id = ? AND distinct_id = ?", projectID, keepExtID); got != 2 {
		t.Errorf("control events = %d, want 2 (must not be over-deleted)", got)
	}

	// Per-person rollups: gone for the subject, present for the control.
	if got := chCount(t, ctx, ch,
		"SELECT count() FROM distinct_id_activity_states WHERE project_id = ? AND distinct_id IN (?, ?)",
		projectID, externalID, anonID); got != 0 {
		t.Errorf("activity states remain: %d", got)
	}
	if got := chCount(t, ctx, ch,
		"SELECT count() FROM distinct_id_activity_states WHERE project_id = ? AND distinct_id = ?",
		projectID, keepExtID); got == 0 {
		t.Error("control activity state was deleted")
	}
	if got := chCount(t, ctx, ch,
		"SELECT count() FROM dashboard_session_rollup WHERE project_id = ? AND toString(session_id) IN (?, ?)",
		projectID, s1, s2); got != 0 {
		t.Errorf("session rollup rows remain: %d", got)
	}
	if got := chCount(t, ctx, ch,
		"SELECT count() FROM dashboard_session_rollup WHERE project_id = ? AND toString(session_id) = ?",
		projectID, s3); got == 0 {
		t.Error("control session rollup was deleted")
	}

	// Profile, aliases, devices: gone (CH + PG); control profile survives.
	if got := chCount(t, ctx, ch, "SELECT count() FROM profiles WHERE project_id = ? AND id = ?", projectID, profileID); got != 0 {
		t.Errorf("ch profile remains: %d", got)
	}
	if got := chCount(t, ctx, ch, "SELECT count() FROM profile_aliases WHERE project_id = ? AND profile_id = ?", projectID, profileID); got != 0 {
		t.Errorf("ch aliases remain: %d", got)
	}
	if got := pgCount(t, ctx, pg, "SELECT count(*) FROM profiles WHERE id = $1", profileID); got != 0 {
		t.Errorf("pg profile remains: %d", got)
	}
	if got := pgCount(t, ctx, pg, "SELECT count(*) FROM profiles WHERE id = $1", keepProfileID); got != 1 {
		t.Errorf("control pg profile = %d, want 1", got)
	}
	if got := pgCount(t, ctx, pg, "SELECT count(*) FROM profile_devices WHERE profile_id = $1", profileID); got != 0 {
		t.Errorf("pg devices remain (orphaned token = PII leak): %d", got)
	}

	// Anonymous aggregate retained, unchanged (decision "a").
	if got := chCount(t, ctx, ch, "SELECT count() FROM dashboard_event_rollup_daily WHERE project_id = ?", projectID); got != rollupBefore {
		t.Errorf("dashboard_event_rollup_daily count = %d, want %d (must be retained, not erased)", got, rollupBefore)
	}

	// Audit row reflects completion with the event count.
	dr, err := svc.GetDeletionRequest(ctx, projectID, requestID)
	if err != nil {
		t.Fatalf("GetDeletionRequest: %v", err)
	}
	if dr.Status != profiles.ComplianceStatusCompleted {
		t.Errorf("request status = %q, want completed", dr.Status)
	}
	if dr.EventsAffected != 5 {
		t.Errorf("events_deleted = %d, want 5", dr.EventsAffected)
	}
	if !dr.CompletedAt.Valid {
		t.Error("completed_at is NULL, want set")
	}

	// Idempotency: re-running the worker is a clean no-op.
	if err := svc.ExecuteErasure(ctx, projectID, requestID); err != nil {
		t.Fatalf("second ExecuteErasure (idempotency): %v", err)
	}
	if got := chCount(t, ctx, ch,
		"SELECT count() FROM events WHERE project_id = ? AND distinct_id IN (?, ?, ?)",
		projectID, externalID, anonID, profileID); got != 0 {
		t.Errorf("events reappeared after re-run: %d", got)
	}
}

// TestErasure_ByExternalIDWithNoProfile pins the no-profile path: events keyed
// directly by an external_id with no profile row must still be erased.
func TestErasure_ByExternalIDWithNoProfile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	ch := testutil.SetupClickHouse(t)
	tn := testutil.SetupNATS(t)
	t.Setenv("NATS_URL", tn.URL)

	natsClient, err := natsdeps.New(ctx)
	if err != nil {
		t.Fatalf("create nats client: %v", err)
	}
	defer natsClient.Close()

	projectID := seedProject(t, ctx, pg)
	now := time.Now().UTC().Truncate(time.Second)
	const ghostID = "ghost@example.com"
	session := uuid.NewString()

	// Events only — no profile row, no alias.
	for range 4 {
		testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, ghostID, "page_view", session,
			map[string]string{}, map[string]string{}, now)
	}

	svc := profiles.NewService(pg.PgW, ch.Conn, natsClient)
	requestID, _, err := svc.RequestErasureByExternalID(ctx, projectID, ghostID, "")
	if err != nil {
		t.Fatalf("RequestErasureByExternalID: %v", err)
	}
	if err := svc.ExecuteErasure(ctx, projectID, requestID); err != nil {
		t.Fatalf("ExecuteErasure: %v", err)
	}

	if got := chCount(t, ctx, ch, "SELECT count() FROM events WHERE project_id = ? AND distinct_id = ?", projectID, ghostID); got != 0 {
		t.Errorf("events remain for no-profile erasure: %d", got)
	}
	if got := chCount(t, ctx, ch,
		"SELECT count() FROM dashboard_session_rollup WHERE project_id = ? AND toString(session_id) = ?", projectID, session); got != 0 {
		t.Errorf("session rollup remains for no-profile erasure: %d", got)
	}
	dr, err := svc.GetDeletionRequest(ctx, projectID, requestID)
	if err != nil {
		t.Fatalf("GetDeletionRequest: %v", err)
	}
	if dr.Status != profiles.ComplianceStatusCompleted {
		t.Errorf("status = %q, want completed", dr.Status)
	}
	if dr.EventsAffected != 4 {
		t.Errorf("events_deleted = %d, want 4", dr.EventsAffected)
	}
	if dr.ProfileID.Valid {
		t.Errorf("profile_id = %q, want NULL (no profile resolved)", dr.ProfileID.String)
	}
}

func seedProject(t *testing.T, ctx context.Context, pg *testutil.TestPostgres) string {
	t.Helper()
	orgID := xid.New().String()
	projectID := xid.New().String()
	if _, err := pg.PgW.Exec(ctx, `INSERT INTO orgs (id, display_name) VALUES ($1, $2)`, orgID, "test-org"); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if _, err := pg.PgW.Exec(ctx,
		`INSERT INTO projects (id, org_id, display_name, private_api_key, public_api_key) VALUES ($1, $2, $3, $4, $5)`,
		projectID, orgID, "test-project", xid.New().String()+"priv", xid.New().String()+"pub",
	); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	return projectID
}

func chCount(t *testing.T, ctx context.Context, ch *testutil.TestClickHouse, query string, args ...any) uint64 {
	t.Helper()
	var n uint64
	if err := ch.Conn.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("ch count %q: %v", query, err)
	}
	return n
}

func pgCount(t *testing.T, ctx context.Context, pg *testutil.TestPostgres, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := pg.PgW.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("pg count %q: %v", query, err)
	}
	return n
}
