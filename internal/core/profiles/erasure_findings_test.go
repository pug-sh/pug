package profiles_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pug-sh/pug/internal/core/profiles"
	natsdeps "github.com/pug-sh/pug/internal/deps/nats"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"github.com/rs/xid"
)

// TestErasure_ExportRowIsNotErasable pins finding C4: the unified ledger holds both
// 'erase' and 'export' rows, and the erase path is scoped to kind = 'erase' in SQL
// (GetEraseRequestByID). A stray/redelivered EraseMessage carrying an export row's
// id must NOT hard-delete the subject, and the erasure-status RPC must not surface
// an export row — both collapse to ErrDeletionRequestNotFound, and the export row
// is left untouched.
func TestErasure_ExportRowIsNotErasable(t *testing.T) {
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
	svc := profiles.NewService(pg.PgW, ch.Conn, natsClient)

	exportID := xid.New().String()
	if _, err := write.CreateComplianceRequest(ctx, dbwrite.CreateComplianceRequestParams{
		ID:         exportID,
		ProjectID:  projectID,
		Kind:       "export",
		ExternalID: postgres.NewText("export-subject@example.com"),
	}); err != nil {
		t.Fatalf("seed export row: %v", err)
	}

	if err := svc.ExecuteErasure(ctx, projectID, exportID); !errors.Is(err, profiles.ErrDeletionRequestNotFound) {
		t.Errorf("ExecuteErasure on export row err = %v, want ErrDeletionRequestNotFound", err)
	}
	if _, err := svc.GetDeletionRequest(ctx, projectID, exportID); !errors.Is(err, profiles.ErrDeletionRequestNotFound) {
		t.Errorf("GetDeletionRequest on export row err = %v, want ErrDeletionRequestNotFound", err)
	}
	if got := pgCount(t, ctx, pg,
		"SELECT count(*) FROM compliance_requests WHERE id = $1 AND kind = 'export' AND status = 'pending'", exportID); got != 1 {
		t.Errorf("export row count = %d, want 1 (must be untouched)", got)
	}
}

// TestErasure_MultiSubjectReopenMatching pins the I5 gap on the dedup keystone:
// GetReopenableComplianceRequest's (profile_id = … OR external_id = …) match must
// re-drive each subject's OWN row, never cross-attach. Two distinct pending
// subjects in one project, then a re-request for each, must resolve to their
// respective rows with no duplicate ledger entries.
func TestErasure_MultiSubjectReopenMatching(t *testing.T) {
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
	svc := profiles.NewService(pg.PgW, ch.Conn, natsClient)

	const extA = "subject-a@example.com"
	const extB = "subject-b@example.com"

	reqA, _, err := svc.RequestErasureByExternalID(ctx, projectID, extA, "")
	if err != nil {
		t.Fatalf("request A: %v", err)
	}
	reqB, _, err := svc.RequestErasureByExternalID(ctx, projectID, extB, "")
	if err != nil {
		t.Fatalf("request B: %v", err)
	}
	if reqA == reqB {
		t.Fatalf("distinct subjects collapsed to one request id %q", reqA)
	}

	// Re-request each subject: each must re-drive ITS OWN row, never the other's.
	reqA2, _, err := svc.RequestErasureByExternalID(ctx, projectID, extA, "")
	if err != nil {
		t.Fatalf("re-request A: %v", err)
	}
	if reqA2 != reqA {
		t.Errorf("re-request A resolved to %q, want %q (cross-attachment)", reqA2, reqA)
	}
	reqB2, _, err := svc.RequestErasureByExternalID(ctx, projectID, extB, "")
	if err != nil {
		t.Fatalf("re-request B: %v", err)
	}
	if reqB2 != reqB {
		t.Errorf("re-request B resolved to %q, want %q (cross-attachment)", reqB2, reqB)
	}

	// Each ledger row carries its own subject identifier.
	drA, err := svc.GetDeletionRequest(ctx, projectID, reqA)
	if err != nil {
		t.Fatalf("get A: %v", err)
	}
	if drA.ExternalID.String != extA {
		t.Errorf("row A external_id = %q, want %q", drA.ExternalID.String, extA)
	}
	drB, err := svc.GetDeletionRequest(ctx, projectID, reqB)
	if err != nil {
		t.Fatalf("get B: %v", err)
	}
	if drB.ExternalID.String != extB {
		t.Errorf("row B external_id = %q, want %q", drB.ExternalID.String, extB)
	}

	// Exactly two rows total: the re-requests reused, never duplicated.
	if got := pgCount(t, ctx, pg,
		"SELECT count(*) FROM compliance_requests WHERE project_id = $1", projectID); got != 2 {
		t.Errorf("ledger rows = %d, want 2", got)
	}
}

// TestComplianceRequests_IdentifierPresentConstraint pins finding I4: a request that
// identifies no subject (both profile_id and external_id NULL) is unrepresentable
// at the storage layer, so the illegal "recorded erasure that can identify nothing"
// state can never be persisted — the runtime ErrNoErasableIdentifiers guard is only
// a backstop.
func TestComplianceRequests_IdentifierPresentConstraint(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	pg := testutil.SetupPostgres(t)
	projectID := seedProject(t, ctx, pg)

	_, err := pg.PgW.Exec(ctx,
		`INSERT INTO compliance_requests (id, project_id, kind) VALUES ($1, $2, 'erase')`,
		xid.New().String(), projectID)
	if err == nil {
		t.Fatal("insert with no identifier succeeded, want CHECK violation")
	}
	if !strings.Contains(err.Error(), "compliance_requests_identifier_present") {
		t.Errorf("err = %v, want compliance_requests_identifier_present violation", err)
	}
}
