package deletion_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pug-sh/pug/internal/core/deletion"
	natsdeps "github.com/pug-sh/pug/internal/deps/nats"
	workercompliancev1 "github.com/pug-sh/pug/internal/gen/proto/workers/compliance/v1"
	"github.com/pug-sh/pug/internal/testutil"
	"github.com/rs/xid"
	"google.golang.org/protobuf/proto"
)

type recordingInvalidator struct {
	blocked    map[string][]string
	unblocked  map[string][]string
	blockErr   error
	unblockErr error
}

type recordingPublisher struct {
	err      error
	subjects []string
	payloads [][]byte
}

func (p *recordingPublisher) Publish(_ context.Context, subject string, payload []byte) error {
	if p.err != nil {
		return p.err
	}
	p.subjects = append(p.subjects, subject)
	p.payloads = append(p.payloads, append([]byte(nil), payload...))
	return nil
}

func newRecordingInvalidator() *recordingInvalidator {
	return &recordingInvalidator{blocked: map[string][]string{}, unblocked: map[string][]string{}}
}

func (r *recordingInvalidator) BlockProjectKeys(_ context.Context, projectID string, tokens ...string) error {
	if r.blockErr != nil {
		return r.blockErr
	}
	r.blocked[projectID] = append([]string(nil), tokens...)
	return nil
}

func (r *recordingInvalidator) UnblockProjectKeys(_ context.Context, projectID string, tokens ...string) error {
	if r.unblockErr != nil {
		return r.unblockErr
	}
	r.unblocked[projectID] = append([]string(nil), tokens...)
	return nil
}

func TestProjectRequestBlocksAccessAndKeepsLedger(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Docker Desktop")
	}
	db := testutil.SetupPostgres(t)
	ctx := context.Background()
	orgID, projectID, actorID := xid.New().String(), xid.New().String(), xid.New().String()
	if _, err := db.PgW.Exec(ctx, `insert into orgs(id,display_name) values($1,'Company')`, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PgW.Exec(ctx, `insert into projects(id,org_id,display_name) values($1,$2,'Web')`, projectID, orgID); err != nil {
		t.Fatal(err)
	}
	const token = "public-key-token"
	if _, err := db.PgW.Exec(ctx, `insert into api_keys(id,kind,masked,project_id,token) values($1,'public',$2,$3,$2)`, xid.New().String(), token, projectID); err != nil {
		t.Fatal(err)
	}
	invalidated := newRecordingInvalidator()
	svc := deletion.NewServiceWithPublisher(db.PgW, invalidated, &recordingPublisher{})
	if _, err := svc.RequestProject(ctx, actorID, orgID, projectID, "Wrong"); !errors.Is(err, deletion.ErrNameMismatch) {
		t.Fatalf("name mismatch: %v", err)
	}
	cacheErr := errors.New("cache unavailable")
	invalidated.blockErr = cacheErr
	if _, err := svc.RequestProject(ctx, actorID, orgID, projectID, "Web"); !errors.Is(err, cacheErr) {
		t.Fatalf("deletion did not fail closed when key block failed: %v", err)
	}
	var state string
	if err := db.PgRO.QueryRow(ctx, `select deletion_state from projects where id=$1`, projectID).Scan(&state); err != nil || state != "active" {
		t.Fatalf("project state after failed key block = %q, %v", state, err)
	}
	invalidated.blockErr = nil
	op, err := svc.RequestProject(ctx, actorID, orgID, projectID, "Web")
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != "pending_deletion" || len(op.Projects) != 1 || op.PurgeAfter.After(time.Now().Add(time.Minute)) {
		t.Fatalf("project deletion operation: %+v", op)
	}
	if tokens := invalidated.blocked[projectID]; len(tokens) != 1 || tokens[0] != token {
		t.Fatalf("blocked project keys: %v", tokens)
	}
	if err := deletion.NewGate(db.PgW).WithActiveProject(ctx, projectID, func(context.Context) error { t.Fatal("inactive project write admitted"); return nil }); !errors.Is(err, deletion.ErrProjectInactive) {
		t.Fatalf("gate: %v", err)
	}
	if repeated, err := svc.RequestProject(ctx, actorID, orgID, projectID, "Web"); err != nil || repeated.ID != op.ID {
		t.Fatalf("duplicate request: %+v, %v", repeated, err)
	}
	otherOrgID := xid.New().String()
	if _, err := db.PgW.Exec(ctx, `insert into orgs(id,display_name) values($1,'Other')`, otherOrgID); err != nil {
		t.Fatal(err)
	}
	if rows, _, err := svc.ListProjectsPage(ctx, otherOrgID, 50, ""); err != nil || len(rows) != 0 {
		t.Fatalf("other org history: %+v, %v", rows, err)
	}
	if rows, _, err := svc.ListProjectsPage(ctx, orgID, 50, ""); err != nil || len(rows) != 1 || rows[0].ID != op.ID {
		t.Fatalf("own org history: %+v, %v", rows, err)
	}
	if _, err := svc.RetryProject(ctx, actorID, otherOrgID, op.ID); !errors.Is(err, deletion.ErrNotFound) {
		t.Fatalf("cross-org retry: %v", err)
	}
	secondID := xid.New().String()
	if _, err := db.PgW.Exec(ctx, `insert into projects(id,org_id,display_name) values($1,$2,'Mobile')`, secondID, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RequestProject(ctx, actorID, orgID, secondID, "Mobile"); err != nil {
		t.Fatal(err)
	}
	first, next, err := svc.ListProjectsPage(ctx, orgID, 1, "")
	if err != nil || len(first) != 1 || next == "" {
		t.Fatalf("first history page: %+v, %q, %v", first, next, err)
	}
	second, final, err := svc.ListProjectsPage(ctx, orgID, 1, next)
	if err != nil || len(second) != 1 || second[0].ID == first[0].ID || final != "" {
		t.Fatalf("second history page: %+v, %q, %v", second, final, err)
	}
	var audited bool
	if err := db.PgRO.QueryRow(ctx, `select exists(select 1 from instance_audit where target_id=$1 and action='project.deletion_requested')`, projectID).Scan(&audited); err != nil || !audited {
		t.Fatalf("request audit: %v, %v", audited, err)
	}
}

func TestProjectEnqueueFailureStaysRetryable(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Docker Desktop")
	}
	db := testutil.SetupPostgres(t)
	ctx := context.Background()
	orgID, projectID, actorID := xid.New().String(), xid.New().String(), xid.New().String()
	if _, err := db.PgW.Exec(ctx, `insert into orgs(id,display_name) values($1,'Company')`, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PgW.Exec(ctx, `insert into projects(id,org_id,display_name) values($1,$2,'Web')`, projectID, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PgW.Exec(ctx, `insert into api_keys(id,kind,masked,project_id,token) values($1,'public','pub_test',$2,'pub_test')`, xid.New().String(), projectID); err != nil {
		t.Fatal(err)
	}
	publishErr := errors.New("nats unavailable")
	publisher := &recordingPublisher{err: publishErr}
	svc := deletion.NewServiceWithPublisher(db.PgW, newRecordingInvalidator(), publisher)
	if _, err := svc.RequestProject(ctx, actorID, orgID, projectID, "Web"); !errors.Is(err, publishErr) {
		t.Fatalf("request error = %v, want publish error", err)
	}
	var operationID, status, projectState string
	if err := db.PgW.QueryRow(ctx, `select id,status from deletion_operations where target_type='project' and target_id=$1`, projectID).Scan(&operationID, &status); err != nil {
		t.Fatal(err)
	}
	if err := db.PgW.QueryRow(ctx, `select deletion_state from projects where id=$1`, projectID).Scan(&projectState); err != nil {
		t.Fatal(err)
	}
	var keys int
	if err := db.PgW.QueryRow(ctx, `select count(*) from api_keys where project_id=$1`, projectID).Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || projectState != "failed" || keys != 0 {
		t.Fatalf("after enqueue failure: operation=%q project=%q keys=%d", status, projectState, keys)
	}

	publisher.err = nil
	if _, err := svc.Retry(ctx, actorID, operationID); err != nil {
		t.Fatal(err)
	}
	if len(publisher.subjects) != 1 || publisher.subjects[0] != natsdeps.ComplianceProjectPurgeSubject {
		t.Fatalf("published subjects = %v", publisher.subjects)
	}
	msg := &workercompliancev1.ProjectPurgeMessage{}
	if err := proto.Unmarshal(publisher.payloads[0], msg); err != nil || msg.GetOperationId() != operationID {
		t.Fatalf("published message = %+v, %v", msg, err)
	}
}

func TestOrganizationEnqueueFailureCanStillBeCancelled(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Docker Desktop")
	}
	db := testutil.SetupPostgres(t)
	ctx := context.Background()
	orgID, projectID, actorID := xid.New().String(), xid.New().String(), xid.New().String()
	if _, err := db.PgW.Exec(ctx, `insert into orgs(id,display_name) values($1,'Company')`, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PgW.Exec(ctx, `insert into projects(id,org_id,display_name) values($1,$2,'Web')`, projectID, orgID); err != nil {
		t.Fatal(err)
	}
	publishErr := errors.New("nats unavailable")
	publisher := &recordingPublisher{err: publishErr}
	svc := deletion.NewServiceWithPublisher(db.PgW, newRecordingInvalidator(), publisher)
	if _, err := svc.RequestOrganization(ctx, actorID, orgID, orgID, "retire tenant"); !errors.Is(err, publishErr) {
		t.Fatalf("request error = %v, want publish error", err)
	}
	var operationID string
	if err := db.PgW.QueryRow(ctx, `select id from deletion_operations where target_type='organization' and target_id=$1`, orgID).Scan(&operationID); err != nil {
		t.Fatal(err)
	}
	cancelled, err := svc.CancelOrganization(ctx, actorID, operationID)
	if err != nil || cancelled.Status != "cancelled" {
		t.Fatalf("cancel enqueue failure = %+v, %v", cancelled, err)
	}
}

func TestOrganizationCancellationWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Docker Desktop")
	}
	db := testutil.SetupPostgres(t)
	ctx := context.Background()
	orgID, projectID, actorID, memberID := xid.New().String(), xid.New().String(), xid.New().String(), xid.New().String()
	if _, err := db.PgW.Exec(ctx, `insert into orgs(id,display_name) values($1,'Company')`, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PgW.Exec(ctx, `insert into customers(id,display_name,email,password_hash,picture_uri) values($1,'Member',$2,'hash','')`, memberID, memberID+"@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PgW.Exec(ctx, `insert into org_members(customer_id,org_id,role) values($1,$2,'ORG_ROLE_ADMIN')`, memberID, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PgW.Exec(ctx, `insert into projects(id,org_id,display_name) values($1,$2,'Web')`, projectID, orgID); err != nil {
		t.Fatal(err)
	}
	const token = "organization-public-key"
	if _, err := db.PgW.Exec(ctx, `insert into api_keys(id,kind,masked,project_id,token) values($1,'public',$2,$3,$2)`, xid.New().String(), token, projectID); err != nil {
		t.Fatal(err)
	}
	invalidated := newRecordingInvalidator()
	svc := deletion.NewServiceWithPublisher(db.PgW, invalidated, &recordingPublisher{})
	if _, err := svc.RequestOrganization(ctx, actorID, orgID, orgID, " "); !errors.Is(err, deletion.ErrReasonRequired) {
		t.Fatalf("blank reason: %v", err)
	}
	op, err := svc.RequestOrganization(ctx, actorID, orgID, orgID, "retire tenant")
	if err != nil {
		t.Fatal(err)
	}
	if op.Status != "pending_deletion" || len(op.Projects) != 1 || op.PurgeAfter.Before(time.Now().Add(23*time.Hour)) || op.PurgeAfter.After(time.Now().Add(25*time.Hour)) {
		t.Fatalf("cancellation window: %+v", op)
	}
	if tokens := invalidated.blocked[projectID]; len(tokens) != 1 || tokens[0] != token {
		t.Fatalf("blocked project keys: %v", tokens)
	}
	var members int
	if err := db.PgW.QueryRow(ctx, `select count(*) from org_members where org_id=$1`, orgID).Scan(&members); err != nil || members != 1 {
		t.Fatalf("members during cancellation window = %d, %v", members, err)
	}
	if repeated, err := svc.RequestOrganization(ctx, actorID, orgID, orgID, "retire tenant"); err != nil || repeated.ID != op.ID {
		t.Fatalf("duplicate organization request: %+v, %v", repeated, err)
	}
	if err := deletion.NewGate(db.PgW).WithActiveProject(ctx, projectID, func(context.Context) error { t.Fatal("pending org write admitted"); return nil }); !errors.Is(err, deletion.ErrProjectInactive) {
		t.Fatalf("gate: %v", err)
	}
	unblockErr := errors.New("cache unavailable")
	invalidated.unblockErr = unblockErr
	if _, err := svc.CancelOrganization(ctx, actorID, op.ID); !errors.Is(err, unblockErr) {
		t.Fatalf("cancellation did not fail closed when key unblock failed: %v", err)
	}
	var state string
	if err := db.PgRO.QueryRow(ctx, `select deletion_state from orgs where id=$1`, orgID).Scan(&state); err != nil || state != "pending_deletion" {
		t.Fatalf("organization state after failed key unblock = %q, %v", state, err)
	}
	invalidated.unblockErr = nil
	cancelled, err := svc.CancelOrganization(ctx, actorID, op.ID)
	if err != nil || cancelled.Status != "cancelled" {
		t.Fatalf("cancel: %+v, %v", cancelled, err)
	}
	if tokens := invalidated.unblocked[projectID]; len(tokens) != 1 || tokens[0] != token {
		t.Fatalf("unblocked project keys: %v", tokens)
	}
	if err := deletion.NewGate(db.PgW).WithActiveProject(ctx, projectID, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("restored project access: %v", err)
	}
	if err := db.PgW.QueryRow(ctx, `select count(*) from org_members where org_id=$1`, orgID).Scan(&members); err != nil || members != 1 {
		t.Fatalf("members after cancellation = %d, %v", members, err)
	}
	if _, err := svc.CancelOrganization(ctx, actorID, op.ID); !errors.Is(err, deletion.ErrCannotCancel) {
		t.Fatalf("duplicate cancellation: %v", err)
	}
}

func TestOrganizationCancellationDeadlineAfterLockWait(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Docker Desktop")
	}
	db := testutil.SetupPostgres(t)
	ctx := context.Background()
	orgID, actorID := xid.New().String(), xid.New().String()
	if _, err := db.PgW.Exec(ctx, `insert into orgs(id,display_name) values($1,'Company')`, orgID); err != nil {
		t.Fatal(err)
	}
	svc := deletion.NewServiceWithPublisher(db.PgW, nil, &recordingPublisher{})
	op, err := svc.RequestOrganization(ctx, actorID, orgID, orgID, "retire tenant")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.PgW.Exec(ctx, `update deletion_operations set purge_after=clock_timestamp()+interval '300 milliseconds' where id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := db.PgW.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `select id from deletion_operations where id=$1 for update`, op.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := svc.CancelOrganization(ctx, actorID, op.ID); done <- err }()
	time.Sleep(450 * time.Millisecond)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, deletion.ErrCannotCancel) {
		t.Fatalf("cancellation succeeded after deadline while waiting for lock: %v", err)
	}
}

func TestDeletionWaitsForInFlightProjectWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Docker Desktop")
	}
	db := testutil.SetupPostgres(t)
	ctx := context.Background()
	orgID, projectID, actorID := xid.New().String(), xid.New().String(), xid.New().String()
	if _, err := db.PgW.Exec(ctx, `insert into orgs(id,display_name) values($1,'Company')`, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PgW.Exec(ctx, `insert into projects(id,org_id,display_name) values($1,$2,'Web')`, projectID, orgID); err != nil {
		t.Fatal(err)
	}
	entered, release, writeDone := make(chan int32), make(chan struct{}), make(chan error, 1)
	go func() {
		writeDone <- deletion.NewGate(db.PgW).WithActiveProjectConnection(ctx, projectID, func(ctx context.Context, conn *pgxpool.Conn) error {
			var backendPID int32
			if err := conn.QueryRow(ctx, `select pg_backend_pid()`).Scan(&backendPID); err != nil {
				return err
			}
			entered <- backendPID
			<-release
			return nil
		})
	}()
	backendPID := <-entered
	var gateState string
	if err := db.PgRO.QueryRow(ctx, `select state from pg_stat_activity where pid=$1`, backendPID).Scan(&gateState); err != nil {
		t.Fatal(err)
	}
	if gateState == "idle in transaction" {
		t.Fatal("deletion gate held an open transaction while the write callback ran")
	}
	requestDone := make(chan error, 1)
	go func() {
		_, err := deletion.NewServiceWithPublisher(db.PgW, nil, &recordingPublisher{}).RequestProject(ctx, actorID, orgID, projectID, "Web")
		requestDone <- err
	}()
	select {
	case err := <-requestDone:
		t.Fatalf("deletion crossed active writer: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	if err := <-requestDone; err != nil {
		t.Fatal(err)
	}
	if err := deletion.NewGate(db.PgW).WithActiveProject(ctx, projectID, func(context.Context) error { t.Fatal("write admitted after deletion"); return nil }); !errors.Is(err, deletion.ErrProjectInactive) {
		t.Fatalf("post-delete gate: %v", err)
	}
}
