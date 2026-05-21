package orgs_test

import (
	"context"
	"errors"
	"testing"

	orgs "github.com/pug-sh/pug/internal/core/orgs"
	orgsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/orgs/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

func TestApplyInviteAcceptanceInTx(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	svc := orgs.NewService(db.PgRO, db.PgW, &stubPublisher{})
	ctx := context.Background()

	inviter, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{ID: "cust-apply-inviter", Email: "apply-inviter@example.com", DisplayName: "Inviter", PasswordHash: "h", PictureUri: ""})
	if err != nil {
		t.Fatalf("CreateCustomer inviter: %v", err)
	}
	org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "org-apply", DisplayName: "Apply Org"})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{OrgID: org.ID, CustomerID: inviter.ID, Role: orgs.RoleAdmin.String()}); err != nil {
		t.Fatalf("CreateOrgMember: %v", err)
	}
	dispatch, err := svc.InviteMemberWithRole(ctx, org.ID, inviter.ID, "apply-invitee@example.com", orgs.RoleMember)
	if err != nil {
		t.Fatalf("InviteMemberWithRole: %v", err)
	}
	invitee, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{ID: "cust-apply-invitee", Email: "apply-invitee@example.com", DisplayName: "Invitee", PasswordHash: "", PictureUri: ""})
	if err != nil {
		t.Fatalf("CreateCustomer invitee: %v", err)
	}

	if err := orgs.ApplyInviteAcceptanceInTx(ctx, write, dispatch.Invitation.ID, invitee.ID); err != nil {
		t.Fatalf("ApplyInviteAcceptanceInTx: %v", err)
	}
	role, err := write.GetOrgMemberRole(ctx, dbwrite.GetOrgMemberRoleParams{OrgID: org.ID, CustomerID: invitee.ID})
	if err != nil || role != orgs.RoleMember.String() {
		t.Fatalf("member role = %q err=%v, want MEMBER", role, err)
	}
	inv, err := write.GetOrgInvitationByIDForUpdate(ctx, dispatch.Invitation.ID)
	if err != nil || inv.Status != orgsv1.InvitationStatus_INVITATION_STATUS_ACCEPTED.String() {
		t.Fatalf("invitation status = %q err=%v, want ACCEPTED", inv.Status, err)
	}

	// Re-applying the now-ACCEPTED invitation → ErrInviteNotPending.
	if err := orgs.ApplyInviteAcceptanceInTx(ctx, write, dispatch.Invitation.ID, invitee.ID); !errors.Is(err, orgs.ErrInviteNotPending) {
		t.Fatalf("second apply err = %v, want ErrInviteNotPending", err)
	}
}

func TestApplyInviteAcceptanceInTx_AlreadyMember(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	svc := orgs.NewService(db.PgRO, db.PgW, &stubPublisher{})
	ctx := context.Background()

	inviter, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{ID: "cust-am-inviter", Email: "am-inviter@example.com", DisplayName: "Inviter", PasswordHash: "h", PictureUri: ""})
	if err != nil {
		t.Fatalf("CreateCustomer inviter: %v", err)
	}
	org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "org-am", DisplayName: "AM Org"})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{OrgID: org.ID, CustomerID: inviter.ID, Role: orgs.RoleAdmin.String()}); err != nil {
		t.Fatalf("CreateOrgMember inviter: %v", err)
	}
	// Create a PENDING invitation while the invitee is NOT yet a member.
	dispatch, err := svc.InviteMemberWithRole(ctx, org.ID, inviter.ID, "am-invitee@example.com", orgs.RoleMember)
	if err != nil {
		t.Fatalf("InviteMemberWithRole: %v", err)
	}
	invitee, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{ID: "cust-am-invitee", Email: "am-invitee@example.com", DisplayName: "Invitee", PasswordHash: "", PictureUri: ""})
	if err != nil {
		t.Fatalf("CreateCustomer invitee: %v", err)
	}
	// Now add them directly so the invite-accept member insert collides.
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{OrgID: org.ID, CustomerID: invitee.ID, Role: orgs.RoleMember.String()}); err != nil {
		t.Fatalf("CreateOrgMember invitee: %v", err)
	}

	if err := orgs.ApplyInviteAcceptanceInTx(ctx, write, dispatch.Invitation.ID, invitee.ID); !errors.Is(err, orgs.ErrAlreadyMember) {
		t.Fatalf("apply err = %v, want ErrAlreadyMember", err)
	}

	// Even though no new member row was created, the invitation must be
	// reconciled to ACCEPTED rather than left dangling as PENDING — the invitee
	// IS a member of the org, however they got there. A PENDING invitation with
	// a consumed token would otherwise linger forever (only recoverable via
	// admin Resend).
	inv, err := write.GetOrgInvitationByIDForUpdate(ctx, dispatch.Invitation.ID)
	if err != nil || inv.Status != orgsv1.InvitationStatus_INVITATION_STATUS_ACCEPTED.String() {
		t.Fatalf("invitation status = %q err=%v, want ACCEPTED", inv.Status, err)
	}
}

func TestApplyInviteAcceptanceInTx_NotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	ctx := context.Background()

	invitee, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{ID: "cust-nf-invitee", Email: "nf-invitee@example.com", DisplayName: "", PasswordHash: "", PictureUri: ""})
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	if err := orgs.ApplyInviteAcceptanceInTx(ctx, write, "no-such-invitation", invitee.ID); !errors.Is(err, orgs.ErrInviteNotFound) {
		t.Fatalf("err = %v, want ErrInviteNotFound", err)
	}
}

// Invitation-level expiry is a distinct check from token expiry: a still-valid
// token whose invitation row has aged past expires_at must yield ErrInviteExpired.
func TestApplyInviteAcceptanceInTx_Expired(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	svc := orgs.NewService(db.PgRO, db.PgW, &stubPublisher{})
	ctx := context.Background()

	inviter, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{ID: "cust-exp-inviter", Email: "exp-inviter@example.com", DisplayName: "", PasswordHash: "h", PictureUri: ""})
	if err != nil {
		t.Fatalf("CreateCustomer inviter: %v", err)
	}
	org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "org-exp", DisplayName: "Exp Org"})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{OrgID: org.ID, CustomerID: inviter.ID, Role: orgs.RoleAdmin.String()}); err != nil {
		t.Fatalf("CreateOrgMember: %v", err)
	}
	dispatch, err := svc.InviteMemberWithRole(ctx, org.ID, inviter.ID, "exp-invitee@example.com", orgs.RoleMember)
	if err != nil {
		t.Fatalf("InviteMemberWithRole: %v", err)
	}
	// Back-date the invitation past its expiry without touching the token.
	if _, err := db.PgW.Exec(ctx, "update org_invitations set expires_at = now() - interval '1 hour' where id = $1", dispatch.Invitation.ID); err != nil {
		t.Fatalf("back-date invitation: %v", err)
	}
	invitee, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{ID: "cust-exp-invitee", Email: "exp-invitee@example.com", DisplayName: "", PasswordHash: "", PictureUri: ""})
	if err != nil {
		t.Fatalf("CreateCustomer invitee: %v", err)
	}
	if err := orgs.ApplyInviteAcceptanceInTx(ctx, write, dispatch.Invitation.ID, invitee.ID); !errors.Is(err, orgs.ErrInviteExpired) {
		t.Fatalf("err = %v, want ErrInviteExpired", err)
	}
}
