package profiles

import (
	"context"
	"errors"
	"strings"
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

	_, err := svc.freezeIdentifiers(context.Background(), &req)
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

	req, err := svc.read.GetEraseRequestByID(ctx, dbread.GetEraseRequestByIDParams{ID: requestID, ProjectID: projectID})
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

// TestErasure_MarkErasureFailedPersistsState pins the I5 gap that every existing
// MarkErasureFailed test used a call-counting fake: this drives the real ledger
// through freeze → 'processing' (the intermediate state asserted nowhere else
// end-to-end) → MarkErasureFailed, and reads back that the row reaches 'failed'
// with the cause truncated to the column bound and the frozen identifiers intact
// (so a later re-drive still cleans every store).
func TestErasure_MarkErasureFailedPersistsState(t *testing.T) {
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

	projectID := seedProjectInternal(t, ctx, pg)
	svc := NewService(pg.PgW, ch.Conn, natsClient)

	const distinctID = "fail-me@example.com"
	session := uuid.NewString()
	requestID := xid.New().String()
	if _, err := svc.write.CreateComplianceRequest(ctx, dbwrite.CreateComplianceRequestParams{
		ID: requestID, ProjectID: projectID, Kind: string(ComplianceKindErase),
		ExternalID: postgres.NewOptionalText(distinctID),
	}); err != nil {
		t.Fatalf("create request: %v", err)
	}
	if _, err := svc.write.FreezeComplianceRequestIdentifiers(ctx, dbwrite.FreezeComplianceRequestIdentifiersParams{
		ID: requestID, ProjectID: projectID,
		DistinctIds: []string{distinctID}, SessionIds: []string{session}, EventsAffected: 7,
	}); err != nil {
		t.Fatalf("freeze identifiers: %v", err)
	}

	// Intermediate-state read-back: freeze advanced the row to 'processing'.
	mid, err := svc.read.GetEraseRequestByID(ctx, dbread.GetEraseRequestByIDParams{ID: requestID, ProjectID: projectID})
	if err != nil {
		t.Fatalf("read after freeze: %v", err)
	}
	if ComplianceStatus(mid.Status) != ComplianceStatusProcessing {
		t.Errorf("status after freeze = %q, want processing", mid.Status)
	}

	// Mark failed with an over-long cause; it must be truncated to maxErrorLen.
	bigCause := errors.New(strings.Repeat("x", 2000))
	if err := svc.MarkErasureFailed(ctx, projectID, requestID, bigCause); err != nil {
		t.Fatalf("MarkErasureFailed: %v", err)
	}

	got, err := svc.read.GetEraseRequestByID(ctx, dbread.GetEraseRequestByIDParams{ID: requestID, ProjectID: projectID})
	if err != nil {
		t.Fatalf("read after fail: %v", err)
	}
	if ComplianceStatus(got.Status) != ComplianceStatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if len(got.Error.String) != 1024 {
		t.Errorf("persisted error len = %d, want 1024 (truncated)", len(got.Error.String))
	}
	if len(got.DistinctIds) != 1 || got.DistinctIds[0] != distinctID {
		t.Errorf("distinct_ids = %v, want frozen [%s] to survive the failure", got.DistinctIds, distinctID)
	}
	if len(got.SessionIds) != 1 || got.SessionIds[0] != session {
		t.Errorf("session_ids = %v, want frozen [%s] to survive the failure", got.SessionIds, session)
	}
}

// TestErasure_FreezeGuardAbortsOnMissingRow pins finding C3: if the freeze UPDATE
// matches 0 rows (the ledger row vanished), freezeIdentifiers must abort with
// ErrComplianceRequestVanished BEFORE any destructive delete — the complement of
// the no-identifiers guard. Here the request row is never created, so the freeze
// write affects 0 rows.
func TestErasure_FreezeGuardAbortsOnMissingRow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	ch := testutil.SetupClickHouse(t)

	svc := NewService(pg.PgW, ch.Conn, nil)
	// External_id present (so resolveDistinctIDs yields one id and the empty-set guard
	// does NOT fire), but no matching ledger row exists, so the freeze hits 0 rows.
	req := dbread.ComplianceRequest{
		ID:         xid.New().String(),
		ProjectID:  xid.New().String(),
		ExternalID: postgres.NewOptionalText("ghost@example.com"),
	}
	_, err := svc.freezeIdentifiers(ctx, &req)
	if !errors.Is(err, ErrComplianceRequestVanished) {
		t.Fatalf("freezeIdentifiers err = %v, want ErrComplianceRequestVanished", err)
	}
}

// TestComplianceRequests_OpenStatusUniqueIndex pins finding I2: the partial unique
// indexes reject a second OPEN (pending/processing) request for the same subject,
// and isComplianceOpenRequestConflict recognizes the violation so the prelude can
// re-drive the winner instead of erroring. Covers both the external_id and
// profile_id indexes.
func TestComplianceRequests_OpenStatusUniqueIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	projectID := seedProjectInternal(t, ctx, pg)
	svc := NewService(pg.PgW, nil, nil)

	cases := []struct {
		name   string
		params dbwrite.CreateComplianceRequestParams
	}{
		{"external_id", dbwrite.CreateComplianceRequestParams{ExternalID: postgres.NewOptionalText("dup@example.com")}},
		{"profile_id", dbwrite.CreateComplianceRequestParams{ProfileID: postgres.NewOptionalText(xid.New().String())}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first := tc.params
			first.ID, first.ProjectID, first.Kind = xid.New().String(), projectID, string(ComplianceKindErase)
			if _, err := svc.write.CreateComplianceRequest(ctx, first); err != nil {
				t.Fatalf("first insert: %v", err)
			}
			second := tc.params
			second.ID, second.ProjectID, second.Kind = xid.New().String(), projectID, string(ComplianceKindErase)
			_, err := svc.write.CreateComplianceRequest(ctx, second)
			if err == nil {
				t.Fatal("second open insert for same subject succeeded, want unique violation")
			}
			if !isComplianceOpenRequestConflict(err) {
				t.Errorf("isComplianceOpenRequestConflict(%v) = false, want true", err)
			}
		})
	}
}

func seedProjectInternal(t *testing.T, ctx context.Context, pg *testutil.TestPostgres) string {
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

func chCountInternal(t *testing.T, ctx context.Context, ch *testutil.TestClickHouse, query string, args ...any) uint64 {
	t.Helper()
	var n uint64
	if err := ch.Conn.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("ch count %q: %v", query, err)
	}
	return n
}
