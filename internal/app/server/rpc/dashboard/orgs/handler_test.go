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
	"github.com/pug-sh/pug/internal/app/server/rpc/dashboard/orgs"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	orgsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/orgs/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

type handlerFixture struct {
	svc           *coreorgs.Service
	write         *dbwrite.Queries
	adminCustomer dbwrite.Customer
	memberCust    dbwrite.Customer
	org           dbwrite.Org
	invitationID  string
	rawToken      string
}

func newHandlerFixture(t *testing.T) *handlerFixture {
	t.Helper()
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	svc := coreorgs.NewService(db.PgRO, db.PgW, nil) // publisher nil — handler tests don't need NATS round-trip
	ctx := context.Background()

	adminID := xid.New().String()
	memberID := xid.New().String()
	orgID := xid.New().String()
	admin, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: adminID, Email: "admin-" + adminID + "@example.com", DisplayName: "Admin", PasswordHash: "h",
	})
	if err != nil {
		t.Fatalf("CreateCustomer admin: %v", err)
	}
	member, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: memberID, Email: "member-" + memberID + "@example.com", DisplayName: "Member", PasswordHash: "h",
	})
	if err != nil {
		t.Fatalf("CreateCustomer member: %v", err)
	}
	org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: orgID, DisplayName: "Handler Test Org"})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID: org.ID, CustomerID: admin.ID, Role: "ORG_ROLE_ADMIN",
	}); err != nil {
		t.Fatalf("CreateOrgMember admin: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID: org.ID, CustomerID: member.ID, Role: "ORG_ROLE_MEMBER",
	}); err != nil {
		t.Fatalf("CreateOrgMember member: %v", err)
	}

	dispatch, err := svc.InviteMember(ctx, org.ID, admin.ID, "invitee-"+orgID+"@example.com")
	if err != nil {
		t.Fatalf("InviteMember: %v", err)
	}
	return &handlerFixture{
		svc: svc, write: write,
		adminCustomer: admin, memberCust: member, org: org,
		invitationID: dispatch.Invitation.ID, rawToken: dispatch.RawToken,
	}
}

func principalCtx(ctx context.Context, c dbwrite.Customer) context.Context {
	return authn.SetInfo(ctx, &rpc.Principal{
		AuthType: rpc.AuthTypeJWT,
		Customer: &dbread.Customer{ID: c.ID, Email: c.Email},
	})
}

func TestResendInviteHandler_HappyPath(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newHandlerFixture(t)
	srv := orgs.NewServer(f.svc)
	resp, err := srv.ResendInvite(
		principalCtx(context.Background(), f.adminCustomer),
		connect.NewRequest(&orgsv1.ResendInviteRequest{
			InvitationId: proto.String(f.invitationID),
			OrgId:        proto.String(f.org.ID),
		}),
	)
	if err != nil {
		t.Fatalf("ResendInvite: %v", err)
	}
	if resp.Msg.GetInvitation().GetId() != f.invitationID {
		t.Fatalf("returned invitation id = %q, want %q", resp.Msg.GetInvitation().GetId(), f.invitationID)
	}
	if got := resp.Msg.GetInvitation().GetStatus(); got != orgsv1.InvitationStatus_INVITATION_STATUS_PENDING {
		t.Fatalf("status = %v, want PENDING (ResendInvite must not advance state)", got)
	}
}

// TestResendInviteHandler_RequiresAdmin pins the admin authz check at the
// handler boundary. A member who is not admin must be rejected before the
// service is touched.
func TestResendInviteHandler_RequiresAdmin(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newHandlerFixture(t)
	srv := orgs.NewServer(f.svc)
	_, err := srv.ResendInvite(
		principalCtx(context.Background(), f.memberCust),
		connect.NewRequest(&orgsv1.ResendInviteRequest{
			InvitationId: proto.String(f.invitationID),
			OrgId:        proto.String(f.org.ID),
		}),
	)
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) || connectErr.Code() != connect.CodePermissionDenied {
		t.Fatalf("expected CodePermissionDenied, got %v", err)
	}
}

// TestResendInviteHandler_UnknownReturnsNotFound pins ErrInviteNotFound →
// CodeNotFound mapping for a bogus invitation_id.
func TestResendInviteHandler_UnknownReturnsNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newHandlerFixture(t)
	srv := orgs.NewServer(f.svc)
	_, err := srv.ResendInvite(
		principalCtx(context.Background(), f.adminCustomer),
		connect.NewRequest(&orgsv1.ResendInviteRequest{
			InvitationId: proto.String(xid.New().String()),
			OrgId:        proto.String(f.org.ID),
		}),
	)
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) || connectErr.Code() != connect.CodeNotFound {
		t.Fatalf("expected CodeNotFound, got %v", err)
	}
}

// TestResendInviteHandler_CrossOrgReturnsNotFound pins that an admin of orgA
// cannot resend an orgB invitation by guessing the invitation_id. The service
// returns ErrInviteNotFound rather than PermissionDenied (anti-enumeration);
// the handler must preserve that and map to CodeNotFound — but only after the
// admin authz check, which passes because the caller IS an admin of otherOrg.
func TestResendInviteHandler_CrossOrgReturnsNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newHandlerFixture(t)
	ctx := context.Background()

	otherOrg, err := f.write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: xid.New().String(), DisplayName: "Other"})
	if err != nil {
		t.Fatalf("CreateOrg other: %v", err)
	}
	if _, err := f.write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID: otherOrg.ID, CustomerID: f.adminCustomer.ID, Role: "ORG_ROLE_ADMIN",
	}); err != nil {
		t.Fatalf("CreateOrgMember other: %v", err)
	}

	srv := orgs.NewServer(f.svc)
	// Admin claims to act on otherOrg, but the invitation_id belongs to f.org.
	_, err = srv.ResendInvite(
		principalCtx(ctx, f.adminCustomer),
		connect.NewRequest(&orgsv1.ResendInviteRequest{
			InvitationId: proto.String(f.invitationID),
			OrgId:        proto.String(otherOrg.ID),
		}),
	)
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) || connectErr.Code() != connect.CodeNotFound {
		t.Fatalf("expected CodeNotFound, got %v", err)
	}
}

// TestResendInviteHandler_AcceptedReturnsFailedPrecondition pins
// ErrInviteNotPending → CodeFailedPrecondition mapping.
func TestResendInviteHandler_AcceptedReturnsFailedPrecondition(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newHandlerFixture(t)
	ctx := context.Background()

	// Create the acceptor as a real customer matching the invitee email so the
	// email-equality guard inside AcceptInvite passes. The invitee email in
	// newHandlerFixture is "invitee-"+orgID+"@example.com" — recompute it here.
	acceptor, err := f.write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: xid.New().String(), Email: "invitee-" + f.org.ID + "@example.com",
		DisplayName: "Acceptor", PasswordHash: "h",
	})
	if err != nil {
		t.Fatalf("CreateCustomer acceptor: %v", err)
	}
	if _, err := f.svc.AcceptInvite(ctx, f.rawToken, acceptor.ID, acceptor.Email); err != nil {
		t.Fatalf("AcceptInvite: %v", err)
	}

	srv := orgs.NewServer(f.svc)
	_, err = srv.ResendInvite(
		principalCtx(ctx, f.adminCustomer),
		connect.NewRequest(&orgsv1.ResendInviteRequest{
			InvitationId: proto.String(f.invitationID),
			OrgId:        proto.String(f.org.ID),
		}),
	)
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) || connectErr.Code() != connect.CodeFailedPrecondition {
		t.Fatalf("expected CodeFailedPrecondition, got %v", err)
	}
}
