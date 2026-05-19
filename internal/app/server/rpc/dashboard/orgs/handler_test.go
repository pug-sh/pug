package orgs_test

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"github.com/rs/xid"
	"google.golang.org/protobuf/proto"

	"github.com/pug-sh/pug/internal/app/server/rpc"
	orgshandler "github.com/pug-sh/pug/internal/app/server/rpc/dashboard/orgs"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	orgsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/orgs/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

func ctxWithCustomer(ctx context.Context, c dbread.Customer) context.Context {
	return authn.SetInfo(ctx, &rpc.Principal{
		AuthType: rpc.AuthTypeJWT,
		Customer: &c,
	})
}

func TestCreateOrgHandlerHappyPath(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgW)
	svc := coreorgs.NewService(db.PgRO, db.PgW, nil)
	srv := orgshandler.NewServer(svc)
	ctx := context.Background()

	id := xid.New().String()
	if _, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID:           id,
		Email:        id + "@example.com",
		DisplayName:  "",
		PictureUri:   "",
		PasswordHash: "x",
	}); err != nil {
		t.Fatalf("seed customer: %v", err)
	}
	customer, err := read.GetCustomerByID(ctx, id)
	if err != nil {
		t.Fatalf("read customer: %v", err)
	}

	resp, err := srv.Create(ctxWithCustomer(ctx, customer), connect.NewRequest(&orgsv1.CreateRequest{
		DisplayName: proto.String("acme"),
	}))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if resp.Msg.GetOrg().GetDisplayName() != "acme" {
		t.Fatalf("want display_name=acme, got %q", resp.Msg.GetOrg().GetDisplayName())
	}
	if resp.Msg.GetOrg().GetRole() != orgsv1.OrgRole_ORG_ROLE_ADMIN {
		t.Fatalf("want role=ADMIN, got %v", resp.Msg.GetOrg().GetRole())
	}
}


func TestLeaveHandlerHappyPath(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgW)
	svc := coreorgs.NewService(db.PgRO, db.PgW, nil)
	srv := orgshandler.NewServer(svc)
	ctx := context.Background()

	ownerID := seedRawCustomer(t, ctx, write, "owner")
	memberID := seedRawCustomer(t, ctx, write, "member")
	org, err := svc.CreateOrgWithDefaults(ctx, ownerID, "leave-handler")
	if err != nil {
		t.Fatalf("seed org: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID: org.ID, CustomerID: memberID, Role: orgsv1.OrgRole_ORG_ROLE_MEMBER.String(),
	}); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	memberCustomer, err := read.GetCustomerByID(ctx, memberID)
	if err != nil {
		t.Fatalf("read member: %v", err)
	}

	if _, err := srv.Leave(
		ctxWithCustomer(ctx, memberCustomer),
		connect.NewRequest(&orgsv1.LeaveRequest{OrgId: proto.String(org.ID)}),
	); err != nil {
		t.Fatalf("Leave: %v", err)
	}
}

func TestLeaveHandlerLastAdminReturnsFailedPrecondition(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgW)
	svc := coreorgs.NewService(db.PgRO, db.PgW, nil)
	srv := orgshandler.NewServer(svc)
	ctx := context.Background()

	adminID := seedRawCustomer(t, ctx, write, "sole-admin")
	memberID := seedRawCustomer(t, ctx, write, "tag-along")
	org, err := svc.CreateOrgWithDefaults(ctx, adminID, "last-admin-handler")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID: org.ID, CustomerID: memberID, Role: orgsv1.OrgRole_ORG_ROLE_MEMBER.String(),
	}); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	admin, err := read.GetCustomerByID(ctx, adminID)
	if err != nil {
		t.Fatalf("read admin: %v", err)
	}
	_, err = srv.Leave(
		ctxWithCustomer(ctx, admin),
		connect.NewRequest(&orgsv1.LeaveRequest{OrgId: proto.String(org.ID)}),
	)
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) || connectErr.Code() != connect.CodeFailedPrecondition {
		t.Fatalf("want CodeFailedPrecondition, got %v", err)
	}
}

func seedRawCustomer(t *testing.T, ctx context.Context, w *dbwrite.Queries, prefix string) string {
	t.Helper()
	id := xid.New().String()
	if _, err := w.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID:           id,
		Email:        prefix + "-" + id + "@example.com",
		DisplayName:  "",
		PictureUri:   "",
		PasswordHash: "x",
	}); err != nil {
		t.Fatalf("seedRawCustomer: %v", err)
	}
	return id
}

func TestUpdateMemberRoleHandlerPromote(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgW)
	svc := coreorgs.NewService(db.PgRO, db.PgW, nil)
	srv := orgshandler.NewServer(svc)
	ctx := context.Background()

	adminID := seedRawCustomer(t, ctx, write, "admin")
	memberID := seedRawCustomer(t, ctx, write, "member")
	org, err := svc.CreateOrgWithDefaults(ctx, adminID, "promote-handler")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID: org.ID, CustomerID: memberID, Role: orgsv1.OrgRole_ORG_ROLE_MEMBER.String(),
	}); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	admin, err := read.GetCustomerByID(ctx, adminID)
	if err != nil {
		t.Fatalf("read admin: %v", err)
	}
	resp, err := srv.UpdateMemberRole(
		ctxWithCustomer(ctx, admin),
		connect.NewRequest(&orgsv1.UpdateMemberRoleRequest{
			OrgId:      proto.String(org.ID),
			CustomerId: proto.String(memberID),
			Role:       orgsv1.OrgRole_ORG_ROLE_ADMIN.Enum(),
		}),
	)
	if err != nil {
		t.Fatalf("UpdateMemberRole: %v", err)
	}
	if resp.Msg.GetMember().GetRole() != orgsv1.OrgRole_ORG_ROLE_ADMIN {
		t.Fatalf("want ADMIN, got %v", resp.Msg.GetMember().GetRole())
	}
}

func TestUpdateMemberRoleHandlerNonAdminRejected(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgW)
	svc := coreorgs.NewService(db.PgRO, db.PgW, nil)
	srv := orgshandler.NewServer(svc)
	ctx := context.Background()

	adminID := seedRawCustomer(t, ctx, write, "real-admin")
	imposterID := seedRawCustomer(t, ctx, write, "imposter")
	targetID := seedRawCustomer(t, ctx, write, "target")
	org, err := svc.CreateOrgWithDefaults(ctx, adminID, "nonadmin-handler")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, id := range []string{imposterID, targetID} {
		if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
			OrgID: org.ID, CustomerID: id, Role: orgsv1.OrgRole_ORG_ROLE_MEMBER.String(),
		}); err != nil {
			t.Fatalf("seed member: %v", err)
		}
	}

	imposter, err := read.GetCustomerByID(ctx, imposterID)
	if err != nil {
		t.Fatalf("read imposter: %v", err)
	}
	_, err = srv.UpdateMemberRole(
		ctxWithCustomer(ctx, imposter),
		connect.NewRequest(&orgsv1.UpdateMemberRoleRequest{
			OrgId:      proto.String(org.ID),
			CustomerId: proto.String(targetID),
			Role:       orgsv1.OrgRole_ORG_ROLE_ADMIN.Enum(),
		}),
	)
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) || connectErr.Code() != connect.CodePermissionDenied {
		t.Fatalf("want CodePermissionDenied, got %v", err)
	}
}

func TestUpdateMemberRoleHandlerDemoteRejected(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgW)
	svc := coreorgs.NewService(db.PgRO, db.PgW, nil)
	srv := orgshandler.NewServer(svc)
	ctx := context.Background()

	adminID := seedRawCustomer(t, ctx, write, "admin")
	coadminID := seedRawCustomer(t, ctx, write, "coadmin")
	org, err := svc.CreateOrgWithDefaults(ctx, adminID, "demote-handler")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID: org.ID, CustomerID: coadminID, Role: orgsv1.OrgRole_ORG_ROLE_ADMIN.String(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	admin, err := read.GetCustomerByID(ctx, adminID)
	if err != nil {
		t.Fatalf("read admin: %v", err)
	}
	_, err = srv.UpdateMemberRole(
		ctxWithCustomer(ctx, admin),
		connect.NewRequest(&orgsv1.UpdateMemberRoleRequest{
			OrgId:      proto.String(org.ID),
			CustomerId: proto.String(coadminID),
			Role:       orgsv1.OrgRole_ORG_ROLE_MEMBER.Enum(),
		}),
	)
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) || connectErr.Code() != connect.CodeInvalidArgument {
		t.Fatalf("want CodeInvalidArgument, got %v", err)
	}
}

func TestLeaveHandlerNonMemberReturnsNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgW)
	svc := coreorgs.NewService(db.PgRO, db.PgW, nil)
	srv := orgshandler.NewServer(svc)
	ctx := context.Background()

	ownerID := seedRawCustomer(t, ctx, write, "owner")
	strangerID := seedRawCustomer(t, ctx, write, "stranger")
	org, err := svc.CreateOrgWithDefaults(ctx, ownerID, "leave-not-member")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	stranger, err := read.GetCustomerByID(ctx, strangerID)
	if err != nil {
		t.Fatalf("read stranger: %v", err)
	}
	_, err = srv.Leave(
		ctxWithCustomer(ctx, stranger),
		connect.NewRequest(&orgsv1.LeaveRequest{OrgId: proto.String(org.ID)}),
	)
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) || connectErr.Code() != connect.CodeNotFound {
		t.Fatalf("want CodeNotFound, got %v", err)
	}
}

func TestListHandlerRoleFieldPerOrg(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgW)
	svc := coreorgs.NewService(db.PgRO, db.PgW, nil)
	srv := orgshandler.NewServer(svc)
	ctx := context.Background()

	// The caller is ADMIN of orgA (created via CreateOrgWithDefaults) and MEMBER
	// of orgB (added via CreateOrgMember after a different customer creates it).
	callerID := seedRawCustomer(t, ctx, write, "caller")
	otherID := seedRawCustomer(t, ctx, write, "other")

	orgA, err := svc.CreateOrgWithDefaults(ctx, callerID, "alpha")
	if err != nil {
		t.Fatalf("seed orgA: %v", err)
	}
	orgB, err := svc.CreateOrgWithDefaults(ctx, otherID, "beta")
	if err != nil {
		t.Fatalf("seed orgB: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID: orgB.ID, CustomerID: callerID, Role: orgsv1.OrgRole_ORG_ROLE_MEMBER.String(),
	}); err != nil {
		t.Fatalf("seed callerID as member of orgB: %v", err)
	}

	caller, err := read.GetCustomerByID(ctx, callerID)
	if err != nil {
		t.Fatalf("read caller: %v", err)
	}

	resp, err := srv.List(
		ctxWithCustomer(ctx, caller),
		connect.NewRequest(&orgsv1.ListRequest{}),
	)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	gotByID := make(map[string]orgsv1.OrgRole, len(resp.Msg.GetOrgs()))
	for _, o := range resp.Msg.GetOrgs() {
		gotByID[o.GetId()] = o.GetRole()
	}
	if got, want := gotByID[orgA.ID], orgsv1.OrgRole_ORG_ROLE_ADMIN; got != want {
		t.Errorf("orgA: want role=%v, got %v", want, got)
	}
	if got, want := gotByID[orgB.ID], orgsv1.OrgRole_ORG_ROLE_MEMBER; got != want {
		t.Errorf("orgB: want role=%v, got %v", want, got)
	}
}
