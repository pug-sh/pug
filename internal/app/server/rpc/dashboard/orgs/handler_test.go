package orgs_test

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/xid"
	"google.golang.org/protobuf/proto"

	"github.com/pug-sh/pug/internal/app/server/rpc"
	orgshandler "github.com/pug-sh/pug/internal/app/server/rpc/dashboard/orgs"
	"github.com/pug-sh/pug/internal/apperr"
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

// orgsBackend bundles the service + query handles that every handler test
// builds; setupOrgsBackend spins up a fresh Postgres and wires them.
type orgsBackend struct {
	svc   *coreorgs.Service
	write *dbwrite.Queries
	read  *dbread.Queries
	pool  *pgxpool.Pool
	ctx   context.Context
}

func setupOrgsBackend(t *testing.T, publisher coreorgs.JobPublisher) orgsBackend {
	t.Helper()
	db := testutil.SetupPostgres(t)
	return orgsBackend{
		svc:   coreorgs.NewService(db.PgRO, db.PgW, publisher),
		write: dbwrite.New(db.PgW),
		read:  dbread.New(db.PgW),
		pool:  db.PgW,
		ctx:   context.Background(),
	}
}

func TestCreateOrgHandlerHappyPath(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	h := setupOrgsBackend(t, nil)
	svc, write, read, ctx := h.svc, h.write, h.read, h.ctx
	srv := orgshandler.NewServer(svc)

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
	h := setupOrgsBackend(t, nil)
	svc, write, read, ctx := h.svc, h.write, h.read, h.ctx
	srv := orgshandler.NewServer(svc)

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
	h := setupOrgsBackend(t, nil)
	svc, write, read, ctx := h.svc, h.write, h.read, h.ctx
	srv := orgshandler.NewServer(svc)

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
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code() != connect.CodeFailedPrecondition {
		t.Fatalf("want apperr CodeFailedPrecondition, got %v (%T)", err, err)
	}
	if ae.Reason() != apperr.ReasonCannotRemoveLastAdmin {
		t.Errorf("reason = %q, want %q", ae.Reason(), apperr.ReasonCannotRemoveLastAdmin)
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
	h := setupOrgsBackend(t, nil)
	svc, write, read, ctx := h.svc, h.write, h.read, h.ctx
	srv := orgshandler.NewServer(svc)

	adminID := seedRawCustomer(t, ctx, write, "admin")
	// Inline the member's customer row so we can assert display_name and email
	// flow into the joined UpdateMemberRole response — the seedRawCustomer
	// helper hardcodes display_name to "" which would mask the joined-field bug.
	memberID := xid.New().String()
	memberEmail := "promoted-" + memberID + "@example.com"
	const memberDisplay = "Member Display"
	if _, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID:           memberID,
		Email:        memberEmail,
		DisplayName:  memberDisplay,
		PictureUri:   "",
		PasswordHash: "x",
	}); err != nil {
		t.Fatalf("seed member: %v", err)
	}
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
	// Pin the full joined response shape so a regression that swapped GetMember
	// for a non-joined query (silently dropping display_name + email) would fail.
	got := resp.Msg.GetMember()
	if got.GetRole() != orgsv1.OrgRole_ORG_ROLE_ADMIN {
		t.Errorf("want ADMIN, got %v", got.GetRole())
	}
	if got.GetCustomerId() != memberID {
		t.Errorf("want customer_id=%q, got %q", memberID, got.GetCustomerId())
	}
	if got.GetOrgId() != org.ID {
		t.Errorf("want org_id=%q, got %q", org.ID, got.GetOrgId())
	}
	if got.GetEmail() != memberEmail {
		t.Errorf("want email=%q, got %q", memberEmail, got.GetEmail())
	}
	if got.GetDisplayName() != memberDisplay {
		t.Errorf("want display_name=%q, got %q", memberDisplay, got.GetDisplayName())
	}
}

func TestUpdateMemberRoleHandlerNonAdminRejected(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	h := setupOrgsBackend(t, nil)
	svc, write, read, ctx := h.svc, h.write, h.read, h.ctx
	srv := orgshandler.NewServer(svc)

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
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code() != connect.CodePermissionDenied {
		t.Fatalf("want apperr CodePermissionDenied, got %v (%T)", err, err)
	}
	if ae.Reason() != apperr.ReasonOrgAdminRequired {
		t.Errorf("reason = %q, want %q", ae.Reason(), apperr.ReasonOrgAdminRequired)
	}
}

func TestUpdateMemberRoleHandlerDemoteRejected(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	h := setupOrgsBackend(t, nil)
	svc, write, read, ctx := h.svc, h.write, h.read, h.ctx
	srv := orgshandler.NewServer(svc)

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
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code() != connect.CodeInvalidArgument {
		t.Fatalf("want apperr CodeInvalidArgument, got %v (%T)", err, err)
	}
	if ae.Reason() != apperr.ReasonOrgUnsupportedRoleTransit {
		t.Errorf("reason = %q, want %q", ae.Reason(), apperr.ReasonOrgUnsupportedRoleTransit)
	}
}

func TestLeaveHandlerNonMemberReturnsNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	h := setupOrgsBackend(t, nil)
	svc, write, read, ctx := h.svc, h.write, h.read, h.ctx
	srv := orgshandler.NewServer(svc)

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
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code() != connect.CodeNotFound {
		t.Fatalf("want apperr CodeNotFound, got %v (%T)", err, err)
	}
	if ae.Reason() != apperr.ReasonOrgMemberNotFound {
		t.Errorf("reason = %q, want %q", ae.Reason(), apperr.ReasonOrgMemberNotFound)
	}
}

func TestInviteMemberHandlerAcceptsRole(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	h := setupOrgsBackend(t, &acceptStubPublisher{})
	svc, write, read, ctx := h.svc, h.write, h.read, h.ctx
	srv := orgshandler.NewServer(svc)

	adminID := seedRawCustomer(t, ctx, write, "role-inviter")
	org, err := svc.CreateOrgWithDefaults(ctx, adminID, "role-invite-handler")
	if err != nil {
		t.Fatalf("seed org: %v", err)
	}
	admin, err := read.GetCustomerByID(ctx, adminID)
	if err != nil {
		t.Fatalf("read admin: %v", err)
	}

	resp, err := srv.InviteMember(
		ctxWithCustomer(ctx, admin),
		connect.NewRequest(&orgsv1.InviteMemberRequest{
			Email: proto.String("role-invitee@example.com"),
			OrgId: proto.String(org.ID),
			Role:  orgsv1.OrgRole_ORG_ROLE_ADMIN.Enum(),
		}),
	)
	if err != nil {
		t.Fatalf("InviteMember: %v", err)
	}
	if got := resp.Msg.GetInvitation().GetRole(); got != orgsv1.OrgRole_ORG_ROLE_ADMIN {
		t.Fatalf("invitation role = %v, want ADMIN", got)
	}
}

// InviteMember with the role field omitted (UNSPECIFIED on the wire) must default
// to ORG_ROLE_MEMBER via inviteRoleFromProto — not be rejected. This is the whole
// reason inviteRoleFromProto differs from roleFromProto.
func TestInviteMemberHandlerDefaultsOmittedRoleToMember(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	h := setupOrgsBackend(t, &acceptStubPublisher{})
	svc, write, read, ctx := h.svc, h.write, h.read, h.ctx
	srv := orgshandler.NewServer(svc)

	adminID := seedRawCustomer(t, ctx, write, "norole-inviter")
	org, err := svc.CreateOrgWithDefaults(ctx, adminID, "norole-invite-handler")
	if err != nil {
		t.Fatalf("seed org: %v", err)
	}
	admin, err := read.GetCustomerByID(ctx, adminID)
	if err != nil {
		t.Fatalf("read admin: %v", err)
	}

	resp, err := srv.InviteMember(
		ctxWithCustomer(ctx, admin),
		connect.NewRequest(&orgsv1.InviteMemberRequest{
			Email: proto.String("norole-invitee@example.com"),
			OrgId: proto.String(org.ID),
			// Role intentionally omitted → UNSPECIFIED → should default to MEMBER.
		}),
	)
	if err != nil {
		t.Fatalf("InviteMember: %v", err)
	}
	if got := resp.Msg.GetInvitation().GetRole(); got != orgsv1.OrgRole_ORG_ROLE_MEMBER {
		t.Fatalf("invitation role = %v, want MEMBER (omitted-role default)", got)
	}
}

// acceptStubPublisher discards published email jobs. Used by invite-related
// handler tests where the email side-effect is not under test.
type acceptStubPublisher struct{}

func (acceptStubPublisher) Publish(_ context.Context, _ string, _ []byte) error { return nil }

// TestGetHandlerReturnsRoleAndMapsNonMemberToNotFound pins both the role
// population on Get and the deliberate enumeration-resistance behavior:
// non-members of an existing org get CodeNotFound (NOT PermissionDenied), so
// an attacker cannot probe org existence by hitting Get with arbitrary IDs.
func TestGetHandlerReturnsRoleAndMapsNonMemberToNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	h := setupOrgsBackend(t, nil)
	svc, write, read, ctx := h.svc, h.write, h.read, h.ctx
	srv := orgshandler.NewServer(svc)

	memberID := seedRawCustomer(t, ctx, write, "member")
	strangerID := seedRawCustomer(t, ctx, write, "stranger")
	org, err := svc.CreateOrgWithDefaults(ctx, memberID, "get-handler")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	memberCustomer, err := read.GetCustomerByID(ctx, memberID)
	if err != nil {
		t.Fatalf("read member: %v", err)
	}
	resp, err := srv.Get(
		ctxWithCustomer(ctx, memberCustomer),
		connect.NewRequest(&orgsv1.GetRequest{OrgId: proto.String(org.ID)}),
	)
	if err != nil {
		t.Fatalf("Get(member): %v", err)
	}
	if got := resp.Msg.GetOrg().GetRole(); got != orgsv1.OrgRole_ORG_ROLE_ADMIN {
		t.Errorf("member view: want role=ADMIN, got %v", got)
	}

	stranger, err := read.GetCustomerByID(ctx, strangerID)
	if err != nil {
		t.Fatalf("read stranger: %v", err)
	}
	_, err = srv.Get(
		ctxWithCustomer(ctx, stranger),
		connect.NewRequest(&orgsv1.GetRequest{OrgId: proto.String(org.ID)}),
	)
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code() != connect.CodeNotFound {
		t.Fatalf("non-member: want apperr CodeNotFound (enumeration-resistant), got %v (%T)", err, err)
	}
	if ae.Reason() != apperr.ReasonOrgNotFound {
		t.Errorf("reason = %q, want %q", ae.Reason(), apperr.ReasonOrgNotFound)
	}
}

// TestGetHandlerReturnsMemberRoleForActualMember pins that Get returns the
// CALLER's role, not the org owner's role. Without this, a regression that
// joined to the wrong row (e.g. always returning the admin's role) would pass
// TestGetHandlerReturnsRoleAndMapsNonMemberToNotFound — which only exercises
// the admin path — but silently mis-report role for member callers.
func TestGetHandlerReturnsMemberRoleForActualMember(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	h := setupOrgsBackend(t, nil)
	svc, write, read, ctx := h.svc, h.write, h.read, h.ctx
	srv := orgshandler.NewServer(svc)

	ownerID := seedRawCustomer(t, ctx, write, "owner")
	memberID := seedRawCustomer(t, ctx, write, "member")
	org, err := svc.CreateOrgWithDefaults(ctx, ownerID, "two-role")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID: org.ID, CustomerID: memberID, Role: orgsv1.OrgRole_ORG_ROLE_MEMBER.String(),
	}); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	member, err := read.GetCustomerByID(ctx, memberID)
	if err != nil {
		t.Fatalf("read member: %v", err)
	}
	resp, err := srv.Get(
		ctxWithCustomer(ctx, member),
		connect.NewRequest(&orgsv1.GetRequest{OrgId: proto.String(org.ID)}),
	)
	if err != nil {
		t.Fatalf("Get(member): %v", err)
	}
	if got := resp.Msg.GetOrg().GetRole(); got != orgsv1.OrgRole_ORG_ROLE_MEMBER {
		t.Errorf("member view: want role=MEMBER, got %v", got)
	}
}

// TestUpdateDisplayNameHandlerReturnsAdminRole pins that the updated-org
// response carries the caller's role (ADMIN, since requireOrgAdmin gates this
// path). A regression that hardcoded UNSPECIFIED would silently drop the
// field for the dashboard.
func TestUpdateDisplayNameHandlerReturnsAdminRole(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	h := setupOrgsBackend(t, nil)
	svc, write, read, ctx := h.svc, h.write, h.read, h.ctx
	srv := orgshandler.NewServer(svc)

	adminID := seedRawCustomer(t, ctx, write, "admin")
	org, err := svc.CreateOrgWithDefaults(ctx, adminID, "old-name")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	admin, err := read.GetCustomerByID(ctx, adminID)
	if err != nil {
		t.Fatalf("read admin: %v", err)
	}

	resp, err := srv.UpdateDisplayName(
		ctxWithCustomer(ctx, admin),
		connect.NewRequest(&orgsv1.UpdateDisplayNameRequest{
			OrgId:       proto.String(org.ID),
			DisplayName: proto.String("new-name"),
		}),
	)
	if err != nil {
		t.Fatalf("UpdateDisplayName: %v", err)
	}
	if got := resp.Msg.GetOrg().GetDisplayName(); got != "new-name" {
		t.Errorf("want display_name=new-name, got %q", got)
	}
	if got := resp.Msg.GetOrg().GetRole(); got != orgsv1.OrgRole_ORG_ROLE_ADMIN {
		t.Errorf("want role=ADMIN (caller is gated by requireOrgAdmin), got %v", got)
	}
}

// TestLeaveHandlerLastMemberReturnsFailedPrecondition mirrors the service
// test TestLeaveNonAdminSoleMember at the handler layer to pin the
// ErrLastMember → CodeFailedPrecondition mapping. The state (non-admin sole
// member) is unreachable through the public API, so we construct it by
// seeding admin+member then directly deleting the admin via the unchecked
// query (same approach as the service-level test).
func TestLeaveHandlerLastMemberReturnsFailedPrecondition(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	h := setupOrgsBackend(t, nil)
	svc, write, read, ctx := h.svc, h.write, h.read, h.ctx
	srv := orgshandler.NewServer(svc)

	adminID := seedRawCustomer(t, ctx, write, "admin")
	lonerID := seedRawCustomer(t, ctx, write, "loner")
	org, err := svc.CreateOrgWithDefaults(ctx, adminID, "last-member")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID: org.ID, CustomerID: lonerID, Role: orgsv1.OrgRole_ORG_ROLE_MEMBER.String(),
	}); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	if _, err := write.DeleteOrgMember(ctx, dbwrite.DeleteOrgMemberParams{
		OrgID: org.ID, CustomerID: adminID,
	}); err != nil {
		t.Fatalf("force-remove admin: %v", err)
	}

	loner, err := read.GetCustomerByID(ctx, lonerID)
	if err != nil {
		t.Fatalf("read loner: %v", err)
	}
	_, err = srv.Leave(
		ctxWithCustomer(ctx, loner),
		connect.NewRequest(&orgsv1.LeaveRequest{OrgId: proto.String(org.ID)}),
	)
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code() != connect.CodeFailedPrecondition {
		t.Fatalf("want apperr CodeFailedPrecondition for ErrLastMember, got %v (%T)", err, err)
	}
	if ae.Reason() != apperr.ReasonCannotLeaveAsLastMember {
		t.Errorf("reason = %q, want %q", ae.Reason(), apperr.ReasonCannotLeaveAsLastMember)
	}
}

func TestListHandlerRoleFieldPerOrg(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	h := setupOrgsBackend(t, nil)
	svc, write, read, ctx := h.svc, h.write, h.read, h.ctx
	srv := orgshandler.NewServer(svc)

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

// TestLeaveHandlerSoloAdminReturnsFailedPrecondition pins the precedence
// rule from service.go:360-363: an admin who is also the only member of
// their org is blocked with ErrLastAdmin (not ErrLastMember). The handler
// maps both to CodeFailedPrecondition but with verb-specific messages —
// this test confirms the "cannot leave as the last admin" message wins
// when both guards would fire.
func TestLeaveHandlerSoloAdminReturnsFailedPrecondition(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	h := setupOrgsBackend(t, nil)
	svc, write, read, ctx := h.svc, h.write, h.read, h.ctx
	srv := orgshandler.NewServer(svc)

	adminID := seedRawCustomer(t, ctx, write, "solo-admin")
	org, err := svc.CreateOrgWithDefaults(ctx, adminID, "solo-admin-leave")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	admin, err := read.GetCustomerByID(ctx, adminID)
	if err != nil {
		t.Fatalf("read admin: %v", err)
	}

	_, err = srv.Leave(
		ctxWithCustomer(ctx, admin),
		connect.NewRequest(&orgsv1.LeaveRequest{OrgId: proto.String(org.ID)}),
	)
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code() != connect.CodeFailedPrecondition {
		t.Fatalf("want apperr CodeFailedPrecondition, got %v (%T)", err, err)
	}
	if ae.Reason() != apperr.ReasonCannotRemoveLastAdmin {
		t.Errorf("reason = %q, want %q", ae.Reason(), apperr.ReasonCannotRemoveLastAdmin)
	}
	// Pin the verb-specific message — confirms ErrLastAdmin precedence won,
	// not ErrLastMember. A regression that swapped the precedence would
	// surface "cannot leave as the only member" here.
	if got, want := ae.Message(), "cannot leave as the last admin"; got != want {
		t.Errorf("want message %q (ErrLastAdmin precedence), got %q", want, got)
	}
}

// TestUpdateMemberRoleHandlerRejectsUnspecifiedRole pins the second-line
// defense in handler.go:373-376: if protovalidate is ever disabled or
// bypassed, the handler still rejects ORG_ROLE_UNSPECIFIED with
// CodeInvalidArgument before reaching the service. Handler tests don't wire
// the protovalidate interceptor, so this path is exercised directly.
func TestUpdateMemberRoleHandlerRejectsUnspecifiedRole(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	h := setupOrgsBackend(t, nil)
	svc, write, read, ctx := h.svc, h.write, h.read, h.ctx
	srv := orgshandler.NewServer(svc)

	adminID := seedRawCustomer(t, ctx, write, "admin")
	memberID := seedRawCustomer(t, ctx, write, "member")
	org, err := svc.CreateOrgWithDefaults(ctx, adminID, "unspec-role")
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

	_, err = srv.UpdateMemberRole(
		ctxWithCustomer(ctx, admin),
		connect.NewRequest(&orgsv1.UpdateMemberRoleRequest{
			OrgId:      proto.String(org.ID),
			CustomerId: proto.String(memberID),
			Role:       orgsv1.OrgRole_ORG_ROLE_UNSPECIFIED.Enum(),
		}),
	)
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code() != connect.CodeInvalidArgument {
		t.Fatalf("want apperr CodeInvalidArgument for UNSPECIFIED role, got %v (%T)", err, err)
	}
	if ae.Reason() != apperr.ReasonOrgUnsupportedRole {
		t.Errorf("reason = %q, want %q", ae.Reason(), apperr.ReasonOrgUnsupportedRole)
	}
}

// TestHandlersRejectUnauthenticated pins that Create/Leave/UpdateMemberRole
// return CodeUnauthenticated when called without a JWT principal in context.
// Handler tests bypass the connect middleware, so the MustGetPrincipal*
// guards at the top of each handler are what fires here.
func TestHandlersRejectUnauthenticated(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	h := setupOrgsBackend(t, nil)
	srv := orgshandler.NewServer(h.svc)
	ctx := context.Background() // intentionally no authn.SetInfo

	cases := []struct {
		name string
		call func() error
	}{
		{"Create", func() error {
			_, err := srv.Create(ctx, connect.NewRequest(&orgsv1.CreateRequest{DisplayName: proto.String("x")}))
			return err
		}},
		{"Leave", func() error {
			_, err := srv.Leave(ctx, connect.NewRequest(&orgsv1.LeaveRequest{OrgId: proto.String("any")}))
			return err
		}},
		{"UpdateMemberRole", func() error {
			_, err := srv.UpdateMemberRole(ctx, connect.NewRequest(&orgsv1.UpdateMemberRoleRequest{
				OrgId: proto.String("any"), CustomerId: proto.String("any"),
				Role: orgsv1.OrgRole_ORG_ROLE_ADMIN.Enum(),
			}))
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			var ae *apperr.Error
			if !errors.As(err, &ae) || ae.Code() != connect.CodeUnauthenticated {
				t.Fatalf("%s: want unauthenticated apperr, got %v (%T)", tc.name, err, err)
			}
			if ae.Reason() != apperr.ReasonUnauthenticated {
				t.Errorf("%s: reason = %q, want %q", tc.name, ae.Reason(), apperr.ReasonUnauthenticated)
			}
		})
	}
}

// TestRemoveMemberHandlerHappyPath: admin removes another member; both stay
// in the DB schema (org_members row gone but customers preserved) and the
// response is empty.
func TestRemoveMemberHandlerHappyPath(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	h := setupOrgsBackend(t, nil)
	svc, write, read, ctx := h.svc, h.write, h.read, h.ctx
	srv := orgshandler.NewServer(svc)

	adminID := seedRawCustomer(t, ctx, write, "admin")
	memberID := seedRawCustomer(t, ctx, write, "to-remove")
	org, err := svc.CreateOrgWithDefaults(ctx, adminID, "remove-happy")
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

	if _, err := srv.RemoveMember(
		ctxWithCustomer(ctx, admin),
		connect.NewRequest(&orgsv1.RemoveMemberRequest{
			OrgId:      proto.String(org.ID),
			CustomerId: proto.String(memberID),
		}),
	); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
}

// TestRemoveMemberHandlerSelfRemovalRejected pins the admin-cannot-remove-self
// guard (handler.go:197-199) mapping to CodeInvalidArgument. The user-facing
// alternative is Leave, which has its own last-admin guard.
func TestRemoveMemberHandlerSelfRemovalRejected(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	h := setupOrgsBackend(t, nil)
	svc, write, read, ctx := h.svc, h.write, h.read, h.ctx
	srv := orgshandler.NewServer(svc)

	adminID := seedRawCustomer(t, ctx, write, "admin")
	org, err := svc.CreateOrgWithDefaults(ctx, adminID, "remove-self")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	admin, err := read.GetCustomerByID(ctx, adminID)
	if err != nil {
		t.Fatalf("read admin: %v", err)
	}

	_, err = srv.RemoveMember(
		ctxWithCustomer(ctx, admin),
		connect.NewRequest(&orgsv1.RemoveMemberRequest{
			OrgId:      proto.String(org.ID),
			CustomerId: proto.String(adminID),
		}),
	)
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code() != connect.CodeInvalidArgument {
		t.Fatalf("want apperr CodeInvalidArgument for self-removal, got %v (%T)", err, err)
	}
	if ae.Reason() != apperr.ReasonOrgCannotRemoveSelf {
		t.Errorf("reason = %q, want %q", ae.Reason(), apperr.ReasonOrgCannotRemoveSelf)
	}
}

// TestRemoveMemberHandlerNotFound pins ErrMemberNotFound → CodeNotFound when
// the admin tries to remove a customer who is not a member of the org.
//
// Note on ErrLastAdmin coverage: a sole-admin attempting to remove themself
// is caught by the InvalidArgument self-removal guard before reaching the
// CTE, and a non-admin cannot pass requireOrgAdmin. ErrLastAdmin via
// RemoveMember is therefore only reachable in a transient concurrent-race
// state covered by the service layer; the handler's FailedPrecondition
// mapping is mirrored by TestLeaveHandlerLastAdminReturnsFailedPrecondition
// which shares the same sentinel.
func TestRemoveMemberHandlerNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	h := setupOrgsBackend(t, nil)
	svc, write, read, ctx := h.svc, h.write, h.read, h.ctx
	srv := orgshandler.NewServer(svc)

	adminID := seedRawCustomer(t, ctx, write, "admin")
	strangerID := seedRawCustomer(t, ctx, write, "stranger")
	org, err := svc.CreateOrgWithDefaults(ctx, adminID, "remove-notfound")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	admin, err := read.GetCustomerByID(ctx, adminID)
	if err != nil {
		t.Fatalf("read admin: %v", err)
	}

	_, err = srv.RemoveMember(
		ctxWithCustomer(ctx, admin),
		connect.NewRequest(&orgsv1.RemoveMemberRequest{
			OrgId:      proto.String(org.ID),
			CustomerId: proto.String(strangerID),
		}),
	)
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code() != connect.CodeNotFound {
		t.Fatalf("want apperr CodeNotFound for non-member removal, got %v (%T)", err, err)
	}
	if ae.Reason() != apperr.ReasonOrgMemberNotFound {
		t.Errorf("reason = %q, want %q", ae.Reason(), apperr.ReasonOrgMemberNotFound)
	}
}

type handlerFixture struct {
	svc           *coreorgs.Service
	write         *dbwrite.Queries
	adminCustomer dbwrite.Customer
	memberCust    dbwrite.Customer
	org           dbwrite.Org
	invitationID  string
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
		invitationID: dispatch.Invitation.ID,
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
	srv := orgshandler.NewServer(f.svc)
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
	srv := orgshandler.NewServer(f.svc)
	_, err := srv.ResendInvite(
		principalCtx(context.Background(), f.memberCust),
		connect.NewRequest(&orgsv1.ResendInviteRequest{
			InvitationId: proto.String(f.invitationID),
			OrgId:        proto.String(f.org.ID),
		}),
	)
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code() != connect.CodePermissionDenied {
		t.Fatalf("want apperr CodePermissionDenied, got %v (%T)", err, err)
	}
	if ae.Reason() != apperr.ReasonOrgAdminRequired {
		t.Errorf("reason = %q, want %q", ae.Reason(), apperr.ReasonOrgAdminRequired)
	}
}

// TestResendInviteHandler_UnknownReturnsNotFound pins ErrInviteNotFound →
// CodeNotFound mapping for a bogus invitation_id.
func TestResendInviteHandler_UnknownReturnsNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newHandlerFixture(t)
	srv := orgshandler.NewServer(f.svc)
	_, err := srv.ResendInvite(
		principalCtx(context.Background(), f.adminCustomer),
		connect.NewRequest(&orgsv1.ResendInviteRequest{
			InvitationId: proto.String(xid.New().String()),
			OrgId:        proto.String(f.org.ID),
		}),
	)
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code() != connect.CodeNotFound {
		t.Fatalf("want apperr CodeNotFound, got %v (%T)", err, err)
	}
	if ae.Reason() != apperr.ReasonInvitationNotFound {
		t.Errorf("reason = %q, want %q", ae.Reason(), apperr.ReasonInvitationNotFound)
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

	srv := orgshandler.NewServer(f.svc)
	// Admin claims to act on otherOrg, but the invitation_id belongs to f.org.
	_, err = srv.ResendInvite(
		principalCtx(ctx, f.adminCustomer),
		connect.NewRequest(&orgsv1.ResendInviteRequest{
			InvitationId: proto.String(f.invitationID),
			OrgId:        proto.String(otherOrg.ID),
		}),
	)
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code() != connect.CodeNotFound {
		t.Fatalf("want apperr CodeNotFound, got %v (%T)", err, err)
	}
	if ae.Reason() != apperr.ReasonInvitationNotFound {
		t.Errorf("reason = %q, want %q", ae.Reason(), apperr.ReasonInvitationNotFound)
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

	// Flip the invitation status directly to ACCEPTED so ResendInvite sees a
	// non-PENDING invitation. Acceptance now flows through magic links
	// (CompleteMagicLink), so we simulate the terminal state via a raw update.
	if _, err := f.write.UpdateOrgInvitationStatus(ctx, dbwrite.UpdateOrgInvitationStatusParams{
		ID:     f.invitationID,
		Status: orgsv1.InvitationStatus_INVITATION_STATUS_ACCEPTED.String(),
	}); err != nil {
		t.Fatalf("flip invitation to ACCEPTED: %v", err)
	}

	srv := orgshandler.NewServer(f.svc)
	_, err := srv.ResendInvite(
		principalCtx(ctx, f.adminCustomer),
		connect.NewRequest(&orgsv1.ResendInviteRequest{
			InvitationId: proto.String(f.invitationID),
			OrgId:        proto.String(f.org.ID),
		}),
	)
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code() != connect.CodeFailedPrecondition {
		t.Fatalf("want apperr CodeFailedPrecondition, got %v (%T)", err, err)
	}
	if ae.Reason() != apperr.ReasonInvitationNotPending {
		t.Errorf("reason = %q, want %q", ae.Reason(), apperr.ReasonInvitationNotPending)
	}
}
