package profiles

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	natsdeps "github.com/pug-sh/pug/internal/deps/nats"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"github.com/rs/xid"
)

// TestFreezeIdentifiers_EmptyRequestRefuses pins the DSAR-correctness guard: a
// request that resolves to no external_id and no profile must fail with
// ErrNoErasableIdentifiers rather than freeze an empty set and let the worker
// mark it 'completed' — a completed erasure that deleted nothing would silently
// misreport fulfilment. A unit test (no infra): an all-empty request never
// reaches ClickHouse, so resolveDistinctIDs returns empty and the guard fires.
func TestFreezeIdentifiers_EmptyRequestRefuses(t *testing.T) {
	svc := NewService(nil, nil, nil)
	req := dbread.ComplianceRequest{ID: "req-empty", ProjectID: "proj-1"}

	_, _, err := svc.freezeIdentifiers(context.Background(), &req)
	if !errors.Is(err, ErrNoErasableIdentifiers) {
		t.Fatalf("freezeIdentifiers err = %v, want ErrNoErasableIdentifiers", err)
	}
}

func TestChInClause(t *testing.T) {
	clause, args := chInClause([]string{"a", "b", "c"})
	if clause != "(?, ?, ?)" {
		t.Errorf("clause = %q, want (?, ?, ?)", clause)
	}
	if len(args) != 3 {
		t.Fatalf("args len = %d, want 3", len(args))
	}
	if args[0] != "a" || args[1] != "b" || args[2] != "c" {
		t.Errorf("args = %v, want [a b c]", args)
	}

	clause, args = chInClause(nil)
	if clause != "()" {
		t.Errorf("empty clause = %q, want ()", clause)
	}
	if len(args) != 0 {
		t.Errorf("empty args len = %d, want 0", len(args))
	}
}

// TestErasure_FrozenIdentifiersSurviveEventDeletion is the crash-recovery
// guarantee that the freeze-on-first-pass design exists for: once a request has
// frozen its session_ids, a retry must use the FROZEN set to erase the session
// rollup even though the events those ids were read from are already deleted (a
// re-resolution would now return nothing). Internal because it sets the frozen
// state directly via the write queries; the external acceptance test only covers
// the clean single-pass + already-completed re-run.
func TestErasure_FrozenIdentifiersSurviveEventDeletion(t *testing.T) {
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

	// Minimal project (the external-package seed helper is not visible here).
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

	now := time.Now().UTC().Truncate(time.Second)
	const distinctID = "frozen@example.com"
	session := uuid.NewString()
	for range 3 {
		testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, distinctID, "page_view", session,
			map[string]string{}, map[string]string{}, now)
	}

	svc := NewService(pg.PgW, ch.Conn, natsClient)

	// Frozen request, no deletes yet — the state after pass 1 froze identifiers
	// but then crashed mid-erase.
	requestID := xid.New().String()
	if _, err := svc.write.CreateComplianceRequest(ctx, dbwrite.CreateComplianceRequestParams{
		ID: requestID, ProjectID: projectID, Kind: string(ComplianceKindErase),
		ExternalID: postgres.NewOptionalText(distinctID),
	}); err != nil {
		t.Fatalf("create request: %v", err)
	}
	if _, err := svc.write.FreezeComplianceRequestIdentifiers(ctx, dbwrite.FreezeComplianceRequestIdentifiersParams{
		ID: requestID, ProjectID: projectID,
		DistinctIds: []string{distinctID}, SessionIds: []string{session}, EventsAffected: 3,
	}); err != nil {
		t.Fatalf("freeze identifiers: %v", err)
	}

	// Simulate "events already deleted by the crashed pass": now a re-resolution
	// of session_ids from events would return nothing.
	if err := svc.execMutation(ctx,
		"ALTER TABLE events DELETE WHERE project_id = ? AND distinct_id = ?", projectID, distinctID); err != nil {
		t.Fatalf("simulate event delete: %v", err)
	}
	if got := chCountInternal(t, ctx, ch, "SELECT count() FROM events WHERE project_id = ? AND distinct_id = ?", projectID, distinctID); got != 0 {
		t.Fatalf("setup: events not deleted: %d", got)
	}
	// The session rollup survives the event delete (insert-triggered MV).
	if got := chCountInternal(t, ctx, ch,
		"SELECT count() FROM dashboard_session_rollup WHERE project_id = ? AND toString(session_id) = ?", projectID, session); got == 0 {
		t.Fatal("setup: session rollup empty; cannot prove frozen-id deletion")
	}

	// Re-drive: must erase the session rollup via the FROZEN session id.
	if err := svc.ExecuteErasure(ctx, projectID, requestID); err != nil {
		t.Fatalf("ExecuteErasure: %v", err)
	}
	if got := chCountInternal(t, ctx, ch,
		"SELECT count() FROM dashboard_session_rollup WHERE project_id = ? AND toString(session_id) = ?", projectID, session); got != 0 {
		t.Errorf("session rollup remains: %d (frozen session id was not used on retry)", got)
	}
}

// TestErasure_PartialEraseRetryCompletes covers the partial-failure contract of
// eraseClickHouse: the mutations run sequentially with no surrounding transaction
// (ClickHouse has none), so a crash after some stores are deleted leaves the rest
// behind. A retry must re-run every (idempotent) mutation off the frozen set and
// drive the request to 'completed'. Here we delete only the events table to
// simulate a first pass that died before clearing the activity-state and session
// rollups, then assert the retry cleans them and marks the row completed.
func TestErasure_PartialEraseRetryCompletes(t *testing.T) {
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

	now := time.Now().UTC().Truncate(time.Second)
	const distinctID = "partial@example.com"
	session := uuid.NewString()
	for range 3 {
		testutil.InsertEvent(ctx, t, ch.Conn, uuid.NewString(), projectID, distinctID, "page_view", session,
			map[string]string{}, map[string]string{}, now)
	}

	svc := NewService(pg.PgW, ch.Conn, natsClient)

	requestID := xid.New().String()
	if _, err := svc.write.CreateComplianceRequest(ctx, dbwrite.CreateComplianceRequestParams{
		ID: requestID, ProjectID: projectID, Kind: string(ComplianceKindErase),
		ExternalID: postgres.NewOptionalText(distinctID),
	}); err != nil {
		t.Fatalf("create request: %v", err)
	}
	if _, err := svc.write.FreezeComplianceRequestIdentifiers(ctx, dbwrite.FreezeComplianceRequestIdentifiersParams{
		ID: requestID, ProjectID: projectID,
		DistinctIds: []string{distinctID}, SessionIds: []string{session}, EventsAffected: 3,
	}); err != nil {
		t.Fatalf("freeze identifiers: %v", err)
	}

	// Simulate a first pass that deleted only the events table before crashing.
	if err := svc.execMutation(ctx,
		"ALTER TABLE events DELETE WHERE project_id = ? AND distinct_id = ?", projectID, distinctID); err != nil {
		t.Fatalf("simulate partial erase: %v", err)
	}
	if got := chCountInternal(t, ctx, ch,
		"SELECT count() FROM distinct_id_activity_states WHERE project_id = ? AND distinct_id = ?", projectID, distinctID); got == 0 {
		t.Fatal("setup: activity state already gone; cannot prove the retry cleans it")
	}

	// Retry: must clean the stores the partial pass left behind and complete.
	if err := svc.ExecuteErasure(ctx, projectID, requestID); err != nil {
		t.Fatalf("ExecuteErasure: %v", err)
	}

	if got := chCountInternal(t, ctx, ch,
		"SELECT count() FROM distinct_id_activity_states WHERE project_id = ? AND distinct_id = ?", projectID, distinctID); got != 0 {
		t.Errorf("activity state remains: %d", got)
	}
	if got := chCountInternal(t, ctx, ch,
		"SELECT count() FROM dashboard_session_rollup WHERE project_id = ? AND toString(session_id) = ?", projectID, session); got != 0 {
		t.Errorf("session rollup remains: %d", got)
	}

	req, err := svc.read.GetComplianceRequestByID(ctx, dbread.GetComplianceRequestByIDParams{ID: requestID, ProjectID: projectID})
	if err != nil {
		t.Fatalf("load request: %v", err)
	}
	if ComplianceStatus(req.Status) != ComplianceStatusCompleted {
		t.Errorf("status = %q, want completed", req.Status)
	}
}

// TestResolveDistinctIDs_DedupsExternalIDEqualToAlias covers the documented edge
// where a profile's external_id coincides with one of its alias_ids: the fan-out
// must collapse them to a single distinct_id rather than emitting a duplicate.
func TestResolveDistinctIDs_DedupsExternalIDEqualToAlias(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	ch := testutil.SetupClickHouse(t)

	projectID := xid.New().String()
	profileID := xid.New().String()
	const externalID = "dup@example.com"

	// An alias whose alias_id collides with the external_id.
	if err := ch.Conn.Exec(ctx,
		`INSERT INTO profile_aliases (alias_id, profile_id, external_id, project_id) VALUES (?, ?, ?, ?)`,
		externalID, profileID, externalID, projectID,
	); err != nil {
		t.Fatalf("seed alias: %v", err)
	}

	svc := NewService(nil, ch.Conn, nil)
	req := dbread.ComplianceRequest{
		ProjectID:  projectID,
		ProfileID:  postgres.NewOptionalText(profileID),
		ExternalID: postgres.NewOptionalText(externalID),
	}
	ids, err := svc.resolveDistinctIDs(ctx, &req)
	if err != nil {
		t.Fatalf("resolveDistinctIDs: %v", err)
	}

	// Expect exactly {externalID, profileID}; the alias (== externalID) is collapsed.
	counts := map[string]int{}
	for _, id := range ids {
		counts[id]++
	}
	if len(ids) != 2 || counts[externalID] != 1 || counts[profileID] != 1 {
		t.Errorf("resolved ids = %v, want [%s %s] with no duplicates", ids, externalID, profileID)
	}
}

func chCountInternal(t *testing.T, ctx context.Context, ch *testutil.TestClickHouse, query string, args ...any) uint64 {
	t.Helper()
	var n uint64
	if err := ch.Conn.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("ch count %q: %v", query, err)
	}
	return n
}
