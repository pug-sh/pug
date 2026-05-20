package projects

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"github.com/rs/xid"
	"google.golang.org/protobuf/proto"

	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/apperr"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	coreprojects "github.com/pug-sh/pug/internal/core/projects"
	projectsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/projects/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

// ctxWithCustomer returns a context with a JWT principal carrying the given customer.
func ctxWithCustomer(ctx context.Context, c dbread.Customer) context.Context {
	return authn.SetInfo(ctx, &rpc.Principal{
		AuthType: rpc.AuthTypeJWT,
		Customer: &c,
	})
}

// ctxWithProject returns a context with a JWT principal carrying the given project.
func ctxWithProject(ctx context.Context, p dbread.Project) context.Context {
	return authn.SetInfo(ctx, &rpc.Principal{
		AuthType: rpc.AuthTypeJWT,
		Project:  &p,
	})
}

func assertCode(t *testing.T, err error, want connect.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var ae *apperr.Error
	if errors.As(err, &ae) {
		if ae.Code() != want {
			t.Errorf("got code %v, want %v (err: %v)", ae.Code(), want, err)
		}
		return
	}
	if got := connect.CodeOf(err); got != want {
		t.Errorf("got code %v, want %v (err: %v)", got, want, err)
	}
}

func assertReason(t *testing.T, err error, want apperr.Reason) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var ae *apperr.Error
	if !errors.As(err, &ae) {
		t.Errorf("expected *apperr.Error to assert reason %q, got %T", want, err)
		return
	}
	if ae.Reason() != want {
		t.Errorf("got reason %q, want %q (err: %v)", ae.Reason(), want, err)
	}
}

// newIntegrationServer sets up a real Postgres + service + handler. Returns
// the handler, a seeded customer, and a seeded org backed by that customer as admin.
func newIntegrationServer(t *testing.T) (*server, dbread.Customer, string) {
	t.Helper()
	db := testutil.SetupPostgres(t)
	projectsSvc := coreprojects.NewService(db.PgRO, db.PgW, nil)
	orgsSvc := coreorgs.NewService(db.PgRO, db.PgW, nil)
	srv := NewServer(projectsSvc, orgsSvc)

	ctx := context.Background()
	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgRO)

	customerID := xid.New().String()
	if _, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID:           customerID,
		Email:        customerID + "@test.example.com",
		DisplayName:  "Test User",
		PictureUri:   "",
		PasswordHash: "x",
	}); err != nil {
		t.Fatalf("seed customer: %v", err)
	}
	customer, err := read.GetCustomerByID(ctx, customerID)
	if err != nil {
		t.Fatalf("read customer: %v", err)
	}

	orgID := xid.New().String()
	if _, err := db.PgW.Exec(ctx,
		`INSERT INTO orgs (id, display_name) VALUES ($1, $2)`,
		orgID, "test-org-"+orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID:      orgID,
		CustomerID: customerID,
		Role:       "ORG_ROLE_ADMIN",
	}); err != nil {
		t.Fatalf("insert org member: %v", err)
	}

	return srv, customer, orgID
}

// ----- Delete: project not found → CodeNotFound + ReasonProjectNotFound ----

func TestHandler_Delete_ProjectNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	db := testutil.SetupPostgres(t)
	projectsSvc := coreprojects.NewService(db.PgRO, db.PgW, nil)
	orgsSvc := coreorgs.NewService(db.PgRO, db.PgW, nil)
	srv := NewServer(projectsSvc, orgsSvc)

	ctx := context.Background()
	orgID := xid.New().String()
	nonexistentProjectID := xid.New().String()

	if _, err := db.PgW.Exec(ctx,
		`INSERT INTO orgs (id, display_name) VALUES ($1, $2)`,
		orgID, "test-org-del"); err != nil {
		t.Fatalf("insert org: %v", err)
	}

	principal := dbread.Project{
		ID:    nonexistentProjectID,
		OrgID: orgID,
	}
	_, err := srv.Delete(
		ctxWithProject(ctx, principal),
		connect.NewRequest(&projectsv1.DeleteRequest{}),
	)
	assertCode(t, err, connect.CodeNotFound)
	assertReason(t, err, apperr.ReasonProjectNotFound)
}

// ----- UpdateDisplayName: project not found → CodeNotFound + ReasonProjectNotFound ----

func TestHandler_UpdateDisplayName_ProjectNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	db := testutil.SetupPostgres(t)
	projectsSvc := coreprojects.NewService(db.PgRO, db.PgW, nil)
	orgsSvc := coreorgs.NewService(db.PgRO, db.PgW, nil)
	srv := NewServer(projectsSvc, orgsSvc)

	ctx := context.Background()
	orgID := xid.New().String()
	nonexistentProjectID := xid.New().String()

	if _, err := db.PgW.Exec(ctx,
		`INSERT INTO orgs (id, display_name) VALUES ($1, $2)`,
		orgID, "test-org-upd"); err != nil {
		t.Fatalf("insert org: %v", err)
	}

	principal := dbread.Project{
		ID:    nonexistentProjectID,
		OrgID: orgID,
	}
	_, err := srv.UpdateDisplayName(
		ctxWithProject(ctx, principal),
		connect.NewRequest(&projectsv1.UpdateDisplayNameRequest{
			DisplayName: proto.String("new name"),
		}),
	)
	assertCode(t, err, connect.CodeNotFound)
	assertReason(t, err, apperr.ReasonProjectNotFound)
}

// ----- Create: admin required → CodePermissionDenied + ReasonOrgAdminRequired ----

func TestHandler_Create_NonAdminReturnsPermissionDenied(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	db := testutil.SetupPostgres(t)
	projectsSvc := coreprojects.NewService(db.PgRO, db.PgW, nil)
	orgsSvc := coreorgs.NewService(db.PgRO, db.PgW, nil)
	srv := NewServer(projectsSvc, orgsSvc)

	ctx := context.Background()
	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgRO)

	// Create a member (not admin) customer.
	memberID := xid.New().String()
	if _, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID:           memberID,
		Email:        memberID + "@test.example.com",
		DisplayName:  "Member User",
		PictureUri:   "",
		PasswordHash: "x",
	}); err != nil {
		t.Fatalf("seed customer: %v", err)
	}
	member, err := read.GetCustomerByID(ctx, memberID)
	if err != nil {
		t.Fatalf("read customer: %v", err)
	}

	orgID := xid.New().String()
	if _, err := db.PgW.Exec(ctx,
		`INSERT INTO orgs (id, display_name) VALUES ($1, $2)`,
		orgID, "test-org-perm"); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID:      orgID,
		CustomerID: memberID,
		Role:       "ORG_ROLE_MEMBER",
	}); err != nil {
		t.Fatalf("insert org member: %v", err)
	}

	_, err = srv.Create(
		ctxWithCustomer(ctx, member),
		connect.NewRequest(&projectsv1.CreateRequest{
			OrgId:       proto.String(orgID),
			DisplayName: proto.String("new project"),
		}),
	)
	assertCode(t, err, connect.CodePermissionDenied)
	assertReason(t, err, apperr.ReasonOrgAdminRequired)
}

// ----- Create: duplicate project name → CodeAlreadyExists + ReasonProjectNameTaken ----

func TestHandler_Create_DuplicateNameReturnsAlreadyExists(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	srv, customer, orgID := newIntegrationServer(t)
	ctx := context.Background()

	_, err := srv.Create(
		ctxWithCustomer(ctx, customer),
		connect.NewRequest(&projectsv1.CreateRequest{
			OrgId:       proto.String(orgID),
			DisplayName: proto.String("my project"),
		}),
	)
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}

	// Second create with the same name should conflict.
	_, err = srv.Create(
		ctxWithCustomer(ctx, customer),
		connect.NewRequest(&projectsv1.CreateRequest{
			OrgId:       proto.String(orgID),
			DisplayName: proto.String("my project"),
		}),
	)
	assertCode(t, err, connect.CodeAlreadyExists)
	assertReason(t, err, apperr.ReasonProjectNameTaken)
}

// ----- BatchGet: not a member → CodePermissionDenied + ReasonOrgNotAMember ----

func TestHandler_BatchGet_NotMemberReturnsPermissionDenied(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	db := testutil.SetupPostgres(t)
	projectsSvc := coreprojects.NewService(db.PgRO, db.PgW, nil)
	orgsSvc := coreorgs.NewService(db.PgRO, db.PgW, nil)
	srv := NewServer(projectsSvc, orgsSvc)

	ctx := context.Background()
	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgRO)

	// Customer who is NOT a member of the org.
	outsiderID := xid.New().String()
	if _, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID:           outsiderID,
		Email:        outsiderID + "@test.example.com",
		DisplayName:  "Outsider",
		PictureUri:   "",
		PasswordHash: "x",
	}); err != nil {
		t.Fatalf("seed customer: %v", err)
	}
	outsider, err := read.GetCustomerByID(ctx, outsiderID)
	if err != nil {
		t.Fatalf("read customer: %v", err)
	}

	orgID := xid.New().String()
	if _, err := db.PgW.Exec(ctx,
		`INSERT INTO orgs (id, display_name) VALUES ($1, $2)`,
		orgID, "test-org-member"); err != nil {
		t.Fatalf("insert org: %v", err)
	}

	_, err = srv.BatchGet(
		ctxWithCustomer(ctx, outsider),
		connect.NewRequest(&projectsv1.BatchGetRequest{
			OrgId: proto.String(orgID),
		}),
	)
	assertCode(t, err, connect.CodePermissionDenied)
	assertReason(t, err, apperr.ReasonOrgNotAMember)
}
