package orgs_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/xid"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/protobuf/proto"

	"github.com/pug-sh/pug/internal/core/orgs"
	natsdeps "github.com/pug-sh/pug/internal/deps/nats"
	orgsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/orgs/v1"
	emailworkerv1 "github.com/pug-sh/pug/internal/gen/proto/workers/email/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

func TestCreateOrgWithDefaultsHappyPath(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgW)
	svc := orgs.NewService(db.PgRO, db.PgW, nil)
	ctx := context.Background()

	customerID := xid.New().String()
	if _, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID:           customerID,
		Email:        customerID + "@example.com",
		DisplayName:  "",
		PictureUri:   "",
		PasswordHash: "x",
	}); err != nil {
		t.Fatalf("seed customer: %v", err)
	}

	org, err := svc.CreateOrgWithDefaults(ctx, customerID, "acme")
	if err != nil {
		t.Fatalf("CreateOrgWithDefaults: %v", err)
	}
	if org.DisplayName != "acme" {
		t.Fatalf("want display_name=acme, got %q", org.DisplayName)
	}

	role, err := read.GetOrgMemberRole(ctx, dbread.GetOrgMemberRoleParams{
		OrgID:      org.ID,
		CustomerID: customerID,
	})
	if err != nil {
		t.Fatalf("GetOrgMemberRole: %v", err)
	}
	if role != orgsv1.OrgRole_ORG_ROLE_ADMIN.String() {
		t.Fatalf("want role=ADMIN, got %q", role)
	}

	projects, err := read.GetProjectsByOrgID(ctx, org.ID)
	if err != nil {
		t.Fatalf("GetProjectsByOrgID: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("want 1 default project, got %d", len(projects))
	}
}

func TestCreateOrgWithDefaultsRollsBackWhenMemberFKFails(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	svc := orgs.NewService(db.PgRO, db.PgW, nil)
	ctx := context.Background()

	// Use a customer that does NOT exist — the CreateOrgMember FK should fail
	// and the whole tx (org + member + project) must roll back.
	missingCustomerID := xid.New().String()

	if _, err := svc.CreateOrgWithDefaults(ctx, missingCustomerID, "rollback-test"); err == nil {
		t.Fatal("want error from CreateOrgMember FK violation, got nil")
	}

	// Assert no org with this display_name exists.
	row := db.PgW.QueryRow(ctx, "select count(*) from orgs where display_name = $1", "rollback-test")
	var n int
	if err := row.Scan(&n); err != nil {
		t.Fatalf("scan count: %v", err)
	}
	if n != 0 {
		t.Fatalf("want 0 orgs after rollback, got %d", n)
	}
}

type stubPublisher struct {
	subject string
	job     *emailworkerv1.EmailJob
	// unmarshalErr surfaces proto round-trip failures separately from
	// publish errors so tests can disambiguate "no publish happened" from
	// "publish happened but the payload was malformed."
	unmarshalErr error
}

func (p *stubPublisher) Publish(_ context.Context, subject string, data []byte) error {
	p.subject = subject
	p.job = &emailworkerv1.EmailJob{}
	p.unmarshalErr = proto.Unmarshal(data, p.job)
	return p.unmarshalErr
}

func TestInviteMemberPublishesEmailJob(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgW)
	pub := &stubPublisher{}
	svc := orgs.NewService(db.PgRO, db.PgW, pub)
	ctx := context.Background()

	customer, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID:           "cust-inviter",
		Email:        "inviter@example.com",
		DisplayName:  "Inviter",
		PasswordHash: "hash",
		PictureUri:   "",
	})
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{
		ID:          "org-test",
		DisplayName: "Test Org",
	})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID:      org.ID,
		CustomerID: customer.ID,
		Role:       "ORG_ROLE_ADMIN",
	}); err != nil {
		t.Fatalf("CreateOrgMember: %v", err)
	}

	dispatch, err := svc.InviteMember(ctx, org.ID, customer.ID, "invitee@example.com")
	if err != nil {
		t.Fatalf("InviteMember: %v", err)
	}
	inv := dispatch.Invitation
	if pub.subject != natsdeps.MiscEmailJobsSubject {
		t.Fatalf("subject = %q, want %q", pub.subject, natsdeps.MiscEmailJobsSubject)
	}
	payload := pub.job.GetOrgMemberInvite()
	if payload == nil {
		t.Fatal("expected org member invite payload")
	}
	if payload.GetInvitationId() != inv.ID {
		t.Fatalf("invitation id = %q, want %q", payload.GetInvitationId(), inv.ID)
	}
	if payload.GetToken() != dispatch.RawToken {
		t.Fatalf("token = %q, want %q", payload.GetToken(), dispatch.RawToken)
	}

	emailToken, err := read.GetValidEmailActionTokenByHashAndPurpose(ctx, dbread.GetValidEmailActionTokenByHashAndPurposeParams{
		TokenHash: hashToken(dispatch.RawToken),
		Purpose:   "org_invite",
	})
	if err != nil {
		t.Fatalf("GetValidEmailActionTokenByHashAndPurpose: %v", err)
	}
	if !emailToken.OrgInvitationID.Valid || emailToken.OrgInvitationID.String != inv.ID {
		t.Fatalf("org invitation id = %v, want %q", emailToken.OrgInvitationID, inv.ID)
	}
	if inv.Token == dispatch.RawToken {
		t.Fatal("invitation row stored a redeemable token")
	}
}

func TestInviteMemberPreservesOtherOrgInviteTokens(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgW)
	pub := &stubPublisher{}
	svc := orgs.NewService(db.PgRO, db.PgW, pub)
	ctx := context.Background()

	customer, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID:           "cust-inviter-2",
		Email:        "inviter2@example.com",
		DisplayName:  "Inviter",
		PasswordHash: "hash",
		PictureUri:   "",
	})
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	orgA, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{
		ID:          "org-a",
		DisplayName: "Org A",
	})
	if err != nil {
		t.Fatalf("CreateOrg orgA: %v", err)
	}
	orgB, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{
		ID:          "org-b",
		DisplayName: "Org B",
	})
	if err != nil {
		t.Fatalf("CreateOrg orgB: %v", err)
	}
	for _, orgID := range []string{orgA.ID, orgB.ID} {
		if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
			OrgID:      orgID,
			CustomerID: customer.ID,
			Role:       "ORG_ROLE_ADMIN",
		}); err != nil {
			t.Fatalf("CreateOrgMember %s: %v", orgID, err)
		}
	}

	firstDispatch, err := svc.InviteMember(ctx, orgA.ID, customer.ID, "invitee@example.com")
	if err != nil {
		t.Fatalf("first InviteMember: %v", err)
	}
	secondDispatch, err := svc.InviteMember(ctx, orgB.ID, customer.ID, "invitee@example.com")
	if err != nil {
		t.Fatalf("second InviteMember: %v", err)
	}

	for name, token := range map[string]string{
		"first":  firstDispatch.RawToken,
		"second": secondDispatch.RawToken,
	} {
		emailToken, err := read.GetValidEmailActionTokenByHashAndPurpose(ctx, dbread.GetValidEmailActionTokenByHashAndPurposeParams{
			TokenHash: hashToken(token),
			Purpose:   "org_invite",
		})
		if err != nil {
			t.Fatalf("%s GetValidEmailActionTokenByHashAndPurpose: %v", name, err)
		}
		if !emailToken.OrgInvitationID.Valid {
			t.Fatalf("%s token missing org invitation id", name)
		}
	}
}

func TestResendInviteRotatesOnlyInvitationToken(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgW)
	pub := &stubPublisher{}
	svc := orgs.NewService(db.PgRO, db.PgW, pub)
	ctx := context.Background()

	customer, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID:           "cust-resend-1",
		Email:        "inviter-resend@example.com",
		DisplayName:  "Inviter",
		PasswordHash: "hash",
		PictureUri:   "",
	})
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	orgA, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "org-resend-a", DisplayName: "Org A"})
	if err != nil {
		t.Fatalf("CreateOrg orgA: %v", err)
	}
	orgB, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "org-resend-b", DisplayName: "Org B"})
	if err != nil {
		t.Fatalf("CreateOrg orgB: %v", err)
	}
	for _, orgID := range []string{orgA.ID, orgB.ID} {
		if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
			OrgID: orgID, CustomerID: customer.ID, Role: "ORG_ROLE_ADMIN",
		}); err != nil {
			t.Fatalf("CreateOrgMember %s: %v", orgID, err)
		}
	}

	firstDispatch, err := svc.InviteMember(ctx, orgA.ID, customer.ID, "invitee@example.com")
	if err != nil {
		t.Fatalf("first InviteMember: %v", err)
	}
	secondDispatch, err := svc.InviteMember(ctx, orgB.ID, customer.ID, "invitee@example.com")
	if err != nil {
		t.Fatalf("second InviteMember: %v", err)
	}

	resendDispatch, err := svc.ResendInvite(ctx, orgA.ID, firstDispatch.Invitation.ID)
	if err != nil {
		t.Fatalf("ResendInvite: %v", err)
	}
	if resendDispatch.Invitation.ID != firstDispatch.Invitation.ID {
		t.Fatalf("resend invitation id = %q, want %q", resendDispatch.Invitation.ID, firstDispatch.Invitation.ID)
	}
	if resendDispatch.RawToken == firstDispatch.RawToken {
		t.Fatal("resend should rotate the raw invite token")
	}
	// Status must remain PENDING — only AcceptInvite advances state.
	if resendDispatch.Invitation.Status != orgsv1.InvitationStatus_INVITATION_STATUS_PENDING.String() {
		t.Fatalf("status = %q, want PENDING", resendDispatch.Invitation.Status)
	}
	if pub.job == nil || pub.job.GetOrgMemberInvite() == nil {
		t.Fatal("expected org invite job on resend")
	}
	if got := pub.job.GetOrgMemberInvite().GetToken(); got != resendDispatch.RawToken {
		t.Fatalf("published token = %q, want %q", got, resendDispatch.RawToken)
	}

	if _, err := read.GetValidEmailActionTokenByHashAndPurpose(ctx, dbread.GetValidEmailActionTokenByHashAndPurposeParams{
		TokenHash: hashToken(firstDispatch.RawToken),
		Purpose:   "org_invite",
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expected old token consumed after resend, got %v", err)
	}
	// GetValid… returns ErrNoRows for both deleted and consumed rows, so verify
	// the prior token row was UPDATEd (consumed_at set) rather than DELETEd —
	// preserving the audit trail. There's no sqlc query for this by design
	// (production code never reads consumed tokens), so we go through the pool.
	var consumedAt pgtype.Timestamptz
	if err := db.PgW.QueryRow(ctx,
		`select consumed_at from email_action_tokens where token_hash = $1`,
		hashToken(firstDispatch.RawToken),
	).Scan(&consumedAt); err != nil {
		t.Fatalf("prior token row missing after resend (expected UPDATE, not DELETE): %v", err)
	}
	if !consumedAt.Valid {
		t.Fatal("prior token row exists but consumed_at is null after resend")
	}
	if _, err := read.GetValidEmailActionTokenByHashAndPurpose(ctx, dbread.GetValidEmailActionTokenByHashAndPurposeParams{
		TokenHash: hashToken(resendDispatch.RawToken),
		Purpose:   "org_invite",
	}); err != nil {
		t.Fatalf("resend token lookup: %v", err)
	}
	if _, err := read.GetValidEmailActionTokenByHashAndPurpose(ctx, dbread.GetValidEmailActionTokenByHashAndPurposeParams{
		TokenHash: hashToken(secondDispatch.RawToken),
		Purpose:   "org_invite",
	}); err != nil {
		t.Fatalf("other org token lookup: %v", err)
	}
}

func TestResendInviteRejectsUnknownInvitation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newInviteFixture(t, "unknown@example.com")
	if _, err := f.svc.ResendInvite(context.Background(), f.org.ID, xid.New().String()); !errors.Is(err, orgs.ErrInviteNotFound) {
		t.Fatalf("expected ErrInviteNotFound, got %v", err)
	}
}

// TestResendInviteRejectsCrossOrg guards the privilege-escalation case where
// an admin of orgA passes orgB's invitation_id. The service must return
// ErrInviteNotFound (anti-enumeration) rather than processing the resend.
func TestResendInviteRejectsCrossOrg(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	svc := orgs.NewService(db.PgRO, db.PgW, &stubPublisher{})
	ctx := context.Background()

	customer, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: "cust-cross-1", Email: "admin-cross@example.com", DisplayName: "Admin", PasswordHash: "h",
	})
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	orgA, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "org-cross-a", DisplayName: "Org A"})
	if err != nil {
		t.Fatalf("CreateOrg orgA: %v", err)
	}
	orgB, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: "org-cross-b", DisplayName: "Org B"})
	if err != nil {
		t.Fatalf("CreateOrg orgB: %v", err)
	}
	for _, orgID := range []string{orgA.ID, orgB.ID} {
		if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
			OrgID: orgID, CustomerID: customer.ID, Role: "ORG_ROLE_ADMIN",
		}); err != nil {
			t.Fatalf("CreateOrgMember %s: %v", orgID, err)
		}
	}

	invB, err := svc.InviteMember(ctx, orgB.ID, customer.ID, "cross-target@example.com")
	if err != nil {
		t.Fatalf("InviteMember orgB: %v", err)
	}

	if _, err := svc.ResendInvite(ctx, orgA.ID, invB.Invitation.ID); !errors.Is(err, orgs.ErrInviteNotFound) {
		t.Fatalf("expected ErrInviteNotFound for cross-org resend, got %v", err)
	}
}

// TestResendInviteRejectsAcceptedInvitation pins that an already-ACCEPTED
// invitation cannot be resurrected via ResendInvite — only PENDING is
// rotatable.
func TestResendInviteRejectsAcceptedInvitation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newInviteFixture(t, "accepted-resend@example.com")
	ctx := context.Background()

	if _, err := f.svc.AcceptInvite(ctx, f.rawToken, f.invitee.ID, "accepted-resend@example.com"); err != nil {
		t.Fatalf("AcceptInvite: %v", err)
	}

	if _, err := f.svc.ResendInvite(ctx, f.org.ID, f.invite.ID); !errors.Is(err, orgs.ErrInviteNotPending) {
		t.Fatalf("expected ErrInviteNotPending, got %v", err)
	}
}

// TestResendInviteExtendsExpiresAt pins the "resurrect expired invite" flow:
// resending a still-PENDING invitation whose expires_at is in the past must
// push expires_at forward by inviteTTL. ResendInvite intentionally does NOT
// gate on expiry — only on status — so an expired pending row can be revived.
func TestResendInviteExtendsExpiresAt(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newInviteFixture(t, "expired-resend@example.com")
	ctx := context.Background()

	if err := backdateInvitation(ctx, t, f, time.Now().Add(-1*time.Hour)); err != nil {
		t.Fatalf("backdateInvitation: %v", err)
	}

	resend, err := f.svc.ResendInvite(ctx, f.org.ID, f.invite.ID)
	if err != nil {
		t.Fatalf("ResendInvite: %v", err)
	}
	// expires_at must be at least 6d23h in the future (inviteTTL=7d, allow a
	// generous skew for slow CI to avoid flake).
	minFuture := time.Now().Add(6*24*time.Hour + 23*time.Hour)
	if !resend.Invitation.ExpiresAt.Valid || resend.Invitation.ExpiresAt.Time.Before(minFuture) {
		t.Fatalf("expires_at not extended: got %v, want after %v", resend.Invitation.ExpiresAt.Time, minFuture)
	}
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// TestInviteMemberRejectsAlreadyMember pins the narrow ErrNoRows→ErrAlreadyMember
// translation in InviteMember (service.go:405-407). The CTE in
// CreateOrgInvitation skips the insert when the email already belongs to an
// existing member; the service must surface that as ErrAlreadyMember rather
// than letting ErrNoRows leak through.
func TestInviteMemberRejectsAlreadyMember(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	svc := orgs.NewService(db.PgRO, db.PgW, &stubPublisher{})
	ctx := context.Background()

	admin := seedCustomer(t, ctx, write, "admin")
	existingMemberEmail := "existing-" + xid.New().String() + "@example.com"
	existingMember, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: xid.New().String(), Email: existingMemberEmail, DisplayName: "Existing", PasswordHash: "x",
	})
	if err != nil {
		t.Fatalf("seed existing member: %v", err)
	}
	org, err := svc.CreateOrgWithDefaults(ctx, admin, "already-member-test")
	if err != nil {
		t.Fatalf("CreateOrgWithDefaults: %v", err)
	}
	mustAddMember(t, ctx, write, org.ID, existingMember.ID, orgsv1.OrgRole_ORG_ROLE_MEMBER.String())

	if _, err := svc.InviteMember(ctx, org.ID, admin, existingMemberEmail); !errors.Is(err, orgs.ErrAlreadyMember) {
		t.Fatalf("want ErrAlreadyMember when inviting existing member, got %v", err)
	}
}

// TestInviteMemberRejectsSecondPendingInvite pins the narrow
// isUniqueViolationOn(orgInvitationsPendingUnique)→ErrInviteAlreadyPending
// translation in InviteMember (service.go:408-410). A second invite to the
// same (org, email) collides on the partial unique index from migration 004
// and must surface as ErrInviteAlreadyPending rather than CodeInternal.
func TestInviteMemberRejectsSecondPendingInvite(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	svc := orgs.NewService(db.PgRO, db.PgW, &stubPublisher{})
	ctx := context.Background()

	admin := seedCustomer(t, ctx, write, "admin")
	org, err := svc.CreateOrgWithDefaults(ctx, admin, "second-invite-test")
	if err != nil {
		t.Fatalf("CreateOrgWithDefaults: %v", err)
	}

	const inviteeEmail = "invitee-once@example.com"
	if _, err := svc.InviteMember(ctx, org.ID, admin, inviteeEmail); err != nil {
		t.Fatalf("first InviteMember: %v", err)
	}
	if _, err := svc.InviteMember(ctx, org.ID, admin, inviteeEmail); !errors.Is(err, orgs.ErrInviteAlreadyPending) {
		t.Fatalf("want ErrInviteAlreadyPending on second invite, got %v", err)
	}
}

// TestGetMemberRoleRejectsDriftedDBValue pins the safety net at the boundary:
// if migration 013's CHECK constraint were ever dropped and a drifted role
// landed in the DB, GetMemberRole's ParseRole must surface an error rather
// than letting Role("ORG_ROLE_OWNER") (or similar) flow through equality
// checks downstream. Setup drops the constraint inside the test's fresh DB.
func TestGetMemberRoleRejectsDriftedDBValue(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	svc := orgs.NewService(db.PgRO, db.PgW, nil)
	ctx := context.Background()

	admin := seedCustomer(t, ctx, write, "admin")
	org, err := svc.CreateOrgWithDefaults(ctx, admin, "drift-test")
	if err != nil {
		t.Fatalf("CreateOrgWithDefaults: %v", err)
	}

	// Temporarily drop the CHECK constraint so we can insert a drifted role.
	// The test's DB is fresh (containerized), so no rollback needed.
	if _, err := db.PgW.Exec(ctx, `alter table org_members drop constraint org_members_role_check`); err != nil {
		t.Fatalf("drop constraint: %v", err)
	}
	if _, err := db.PgW.Exec(ctx,
		`update org_members set role = 'ORG_ROLE_OWNER' where org_id = $1 and customer_id = $2`,
		org.ID, admin,
	); err != nil {
		t.Fatalf("inject drifted role: %v", err)
	}

	if _, err := svc.GetMemberRole(ctx, org.ID, admin); err == nil {
		t.Fatal("want error for drifted role, got nil")
	}
}

// TestMigration013RejectsInvalidRole pins the DB-side CHECK constraint
// from migration 013 directly: any attempt to insert a role outside the
// recognized set must fail at the database level, regardless of what the
// application thinks.
func TestMigration013RejectsInvalidRole(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	ctx := context.Background()

	cust := seedCustomer(t, ctx, write, "cust")
	org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{
		ID: xid.New().String(), DisplayName: "check-test",
	})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}

	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID: org.ID, CustomerID: cust, Role: "ORG_ROLE_OWNER",
	}); err == nil {
		t.Fatal("want CHECK violation for invalid role, got nil")
	}
}

// TestAddMemberRejectsDuplicate pins the narrow
// isUniqueViolationOn(orgMembersPKey)→ErrAlreadyMember translation in
// AddMember (service.go:185-187). A second AddMember for the same
// (org_id, customer_id) collides on the composite primary key.
func TestAddMemberRejectsDuplicate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	svc := orgs.NewService(db.PgRO, db.PgW, nil)
	ctx := context.Background()

	admin := seedCustomer(t, ctx, write, "admin")
	other := seedCustomer(t, ctx, write, "other")
	org, err := svc.CreateOrgWithDefaults(ctx, admin, "addmember-dup-test")
	if err != nil {
		t.Fatalf("CreateOrgWithDefaults: %v", err)
	}

	if _, err := svc.AddMember(ctx, org.ID, other, orgs.RoleMember); err != nil {
		t.Fatalf("first AddMember: %v", err)
	}
	if _, err := svc.AddMember(ctx, org.ID, other, orgs.RoleMember); !errors.Is(err, orgs.ErrAlreadyMember) {
		t.Fatalf("want ErrAlreadyMember on second AddMember, got %v", err)
	}
}

// inviteFixture sets up an inviter customer + org + invitee customer +
// pending invitation, and returns the raw invite token. Centralises the
// boilerplate used by the AcceptInvite test suite below.
type inviteFixture struct {
	t        *testing.T
	svc      *orgs.Service
	pool     *pgxpool.Pool
	write    *dbwrite.Queries
	read     *dbread.Queries
	org      dbwrite.Org
	invitee  dbwrite.Customer
	inviter  dbwrite.Customer
	invite   dbwrite.OrgInvitation
	rawToken string
}

func newInviteFixture(t *testing.T, inviteeEmail string) *inviteFixture {
	t.Helper()
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	read := dbread.New(db.PgW)
	svc := orgs.NewService(db.PgRO, db.PgW, &stubPublisher{})
	ctx := context.Background()

	inviter, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: xid.New().String(), Email: "inviter-" + xid.New().String() + "@example.com",
		DisplayName: "Inviter", PasswordHash: "hash",
	})
	if err != nil {
		t.Fatalf("CreateCustomer inviter: %v", err)
	}
	invitee, err := write.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID: xid.New().String(), Email: inviteeEmail,
		DisplayName: "Invitee", PasswordHash: "hash",
	})
	if err != nil {
		t.Fatalf("CreateCustomer invitee: %v", err)
	}
	org, err := write.CreateOrg(ctx, dbwrite.CreateOrgParams{
		ID: xid.New().String(), DisplayName: "Acme",
	})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := write.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID: org.ID, CustomerID: inviter.ID, Role: "ORG_ROLE_ADMIN",
	}); err != nil {
		t.Fatalf("CreateOrgMember inviter: %v", err)
	}

	dispatch, err := svc.InviteMember(context.Background(), org.ID, inviter.ID, inviteeEmail)
	if err != nil {
		t.Fatalf("InviteMember: %v", err)
	}
	return &inviteFixture{
		t: t, svc: svc, pool: db.PgW, write: write, read: read,
		org: org, invitee: invitee, inviter: inviter, invite: dispatch.Invitation, rawToken: dispatch.RawToken,
	}
}

// TestAcceptInviteHappyPath pins the redesigned two-step accept flow
// (email_action_token → invitation). On success: org returned, status →
// ACCEPTED, email-action token consumed, replay rejected, invitee is now a
// member with the MEMBER role.
func TestAcceptInviteHappyPath(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	const email = "happy@example.com"
	f := newInviteFixture(t, email)
	ctx := context.Background()

	org, err := f.svc.AcceptInvite(ctx, f.rawToken, f.invitee.ID, email)
	if err != nil {
		t.Fatalf("AcceptInvite: %v", err)
	}
	if org.ID != f.org.ID {
		t.Fatalf("returned org id = %q, want %q", org.ID, f.org.ID)
	}

	// Status flipped to ACCEPTED.
	updated, err := f.write.GetOrgInvitationByIDForUpdate(ctx, f.invite.ID)
	if err != nil {
		t.Fatalf("GetOrgInvitationByIDForUpdate: %v", err)
	}
	if updated.Status != orgsv1.InvitationStatus_INVITATION_STATUS_ACCEPTED.String() {
		t.Fatalf("status = %q, want ACCEPTED", updated.Status)
	}

	// Email-action token consumed → cannot be looked up via the "valid" query.
	if _, err := f.read.GetValidEmailActionTokenByHashAndPurpose(ctx, dbread.GetValidEmailActionTokenByHashAndPurposeParams{
		TokenHash: hashToken(f.rawToken), Purpose: "org_invite",
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expected token consumed (ErrNoRows), got %v", err)
	}

	// Replay must fail.
	if _, err := f.svc.AcceptInvite(ctx, f.rawToken, f.invitee.ID, email); !errors.Is(err, orgs.ErrInviteNotFound) {
		t.Fatalf("replay: expected ErrInviteNotFound, got %v", err)
	}
}

// TestAcceptInviteRejectsWrongEmail pins the email-equality guard. The token
// is valid but the customer claiming it has a different email.
func TestAcceptInviteRejectsWrongEmail(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	f := newInviteFixture(t, "issued-to@example.com")
	if _, err := f.svc.AcceptInvite(context.Background(), f.rawToken, f.invitee.ID, "different@example.com"); !errors.Is(err, orgs.ErrInviteWrongEmail) {
		t.Fatalf("expected ErrInviteWrongEmail, got %v", err)
	}
}

// TestAcceptInviteRejectsAlreadyAccepted pins that the second accept against
// an invitation already redeemed returns ErrInviteNotFound (because the token
// is consumed) — NOT a confusing "already member" error. The replay
// rejection happens at the token lookup, before reaching CreateOrgMember.
func TestAcceptInviteRejectsAlreadyAccepted(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	const email = "twice@example.com"
	f := newInviteFixture(t, email)
	ctx := context.Background()

	if _, err := f.svc.AcceptInvite(ctx, f.rawToken, f.invitee.ID, email); err != nil {
		t.Fatalf("first AcceptInvite: %v", err)
	}
	if _, err := f.svc.AcceptInvite(ctx, f.rawToken, f.invitee.ID, email); !errors.Is(err, orgs.ErrInviteNotFound) {
		t.Fatalf("second AcceptInvite: expected ErrInviteNotFound, got %v", err)
	}
}

// TestAcceptInviteRejectsInvalidToken pins that a token that was never issued
// is rejected at the email_action_token lookup. A bare `xid.New().String()`
// will hash to a value with no row.
func TestAcceptInviteRejectsInvalidToken(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	const email = "nobody@example.com"
	f := newInviteFixture(t, email)
	bogus := xid.New().String() + xid.New().String()
	if _, err := f.svc.AcceptInvite(context.Background(), bogus, f.invitee.ID, email); !errors.Is(err, orgs.ErrInviteNotFound) {
		t.Fatalf("expected ErrInviteNotFound for bogus token, got %v", err)
	}
}

// TestAcceptInviteRejectsExpiredInvitation pins the time-window check by
// forcibly back-dating the invitation row. The token row stays valid (its
// own ExpiresAt is independent), so we reach the inv.ExpiresAt comparison
// inside AcceptInvite and hit ErrInviteExpired.
func TestAcceptInviteRejectsExpiredInvitation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	const email = "stale@example.com"
	f := newInviteFixture(t, email)
	ctx := context.Background()

	// Force the invitation's ExpiresAt into the past via a raw UPDATE on
	// the underlying pool. We do not have a sqlc helper for this because
	// invites are never legitimately back-dated — that's the point.
	if _, err := f.write.UpdateOrgInvitationStatus(ctx, dbwrite.UpdateOrgInvitationStatusParams{
		ID:     f.invite.ID,
		Status: orgsv1.InvitationStatus_INVITATION_STATUS_PENDING.String(),
	}); err != nil {
		t.Fatalf("seed UpdateOrgInvitationStatus: %v", err)
	}
	if err := backdateInvitation(ctx, t, f, time.Now().Add(-1*time.Hour)); err != nil {
		t.Fatalf("backdateInvitation: %v", err)
	}

	if _, err := f.svc.AcceptInvite(ctx, f.rawToken, f.invitee.ID, email); !errors.Is(err, orgs.ErrInviteExpired) {
		t.Fatalf("expected ErrInviteExpired, got %v", err)
	}
}

// TestAcceptInviteRejectsStaleRawTokenAfterResend pins defense-in-depth on the
// invite-token rotation invariant: a raw token captured from an earlier invite
// link must not redeem after a legitimate ResendInvite. The mechanism is
// already covered indirectly by the GetValid → ErrNoRows assertion in
// TestResendInviteRotatesOnlyInvitationToken, but this exercises the full
// AcceptInvite path so a refactor that bypassed GetValid would still fail.
func TestAcceptInviteRejectsStaleRawTokenAfterResend(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	const email = "stale-after-resend@example.com"
	f := newInviteFixture(t, email)
	ctx := context.Background()
	staleToken := f.rawToken

	if _, err := f.svc.ResendInvite(ctx, f.org.ID, f.invite.ID); err != nil {
		t.Fatalf("ResendInvite: %v", err)
	}

	if _, err := f.svc.AcceptInvite(ctx, staleToken, f.invitee.ID, email); !errors.Is(err, orgs.ErrInviteNotFound) {
		t.Fatalf("expected ErrInviteNotFound for stale token after resend, got %v", err)
	}
}

// backdateInvitation overrides a pending invitation's expires_at via raw SQL.
// Used only by the expired-invitation test — there's no production code path
// to back-date, which is exactly why we bypass sqlc.
func backdateInvitation(ctx context.Context, t *testing.T, f *inviteFixture, ts time.Time) error {
	t.Helper()
	_, err := f.pool.Exec(ctx, `update org_invitations set expires_at = $1 where id = $2`,
		pgtype.Timestamptz{Time: ts, Valid: true}, f.invite.ID)
	return err
}

func seedCustomer(t *testing.T, ctx context.Context, w *dbwrite.Queries, prefix string) string {
	t.Helper()
	id := xid.New().String()
	if _, err := w.CreateCustomer(ctx, dbwrite.CreateCustomerParams{
		ID:           id,
		Email:        prefix + "-" + id + "@example.com",
		DisplayName:  "",
		PictureUri:   "",
		PasswordHash: "x",
	}); err != nil {
		t.Fatalf("seedCustomer: %v", err)
	}
	return id
}

func mustAddMember(t *testing.T, ctx context.Context, w *dbwrite.Queries, orgID, customerID, role string) {
	t.Helper()
	if _, err := w.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{
		OrgID:      orgID,
		CustomerID: customerID,
		Role:       role,
	}); err != nil {
		t.Fatalf("mustAddMember: %v", err)
	}
}

func TestLookupInviteEmailInTx(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	ctx := context.Background()
	w := dbwrite.New(db.PgW)
	svc := orgs.NewService(db.PgRO, db.PgW, &stubPublisher{})

	inviterID := xid.New().String()
	if _, err := w.CreateCustomer(ctx, dbwrite.CreateCustomerParams{ID: inviterID, Email: "inv-" + inviterID + "@example.com", DisplayName: "I", PasswordHash: "h"}); err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	org, err := w.CreateOrg(ctx, dbwrite.CreateOrgParams{ID: xid.New().String(), DisplayName: "Acme"})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := w.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{OrgID: org.ID, CustomerID: inviterID, Role: orgsv1.OrgRole_ORG_ROLE_ADMIN.String()}); err != nil {
		t.Fatalf("CreateOrgMember: %v", err)
	}
	inviteeEmail := "invitee-" + xid.New().String() + "@example.com"
	inv, err := svc.InviteMember(ctx, org.ID, inviterID, inviteeEmail)
	if err != nil {
		t.Fatalf("InviteMember: %v", err)
	}

	got, err := orgs.LookupInviteEmailInTx(ctx, dbread.New(db.PgW), inv.RawToken)
	if err != nil {
		t.Fatalf("LookupInviteEmailInTx: %v", err)
	}
	if got != inviteeEmail {
		t.Errorf("email = %q, want %q", got, inviteeEmail)
	}

	if _, err := orgs.LookupInviteEmailInTx(ctx, dbread.New(db.PgW), "no-such-token"); !errors.Is(err, orgs.ErrInviteNotFound) {
		t.Errorf("bad token err = %v, want ErrInviteNotFound", err)
	}

	// A consumed (accepted) token resolves to ErrInviteNotFound.
	acceptorID := xid.New().String()
	if _, err := w.CreateCustomer(ctx, dbwrite.CreateCustomerParams{ID: acceptorID, Email: inviteeEmail, DisplayName: "A", PasswordHash: "h"}); err != nil {
		t.Fatalf("CreateCustomer acceptor: %v", err)
	}
	if _, err := svc.AcceptInvite(ctx, inv.RawToken, acceptorID, inviteeEmail); err != nil {
		t.Fatalf("AcceptInvite: %v", err)
	}
	if _, err := orgs.LookupInviteEmailInTx(ctx, dbread.New(db.PgW), inv.RawToken); !errors.Is(err, orgs.ErrInviteNotFound) {
		t.Errorf("consumed token err = %v, want ErrInviteNotFound", err)
	}
}

func TestLeaveHappyPath(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	svc := orgs.NewService(db.PgRO, db.PgW, nil)
	ctx := context.Background()

	owner := seedCustomer(t, ctx, write, "owner")
	mate := seedCustomer(t, ctx, write, "mate")
	leaver := seedCustomer(t, ctx, write, "leaver")
	org, err := svc.CreateOrgWithDefaults(ctx, owner, "leave-org")
	if err != nil {
		t.Fatalf("seed org: %v", err)
	}
	mustAddMember(t, ctx, write, org.ID, mate, orgsv1.OrgRole_ORG_ROLE_ADMIN.String())
	mustAddMember(t, ctx, write, org.ID, leaver, orgsv1.OrgRole_ORG_ROLE_MEMBER.String())

	if err := svc.Leave(ctx, org.ID, leaver); err != nil {
		t.Fatalf("Leave: %v", err)
	}

	if _, err := write.GetOrgMemberRole(ctx, dbwrite.GetOrgMemberRoleParams{
		OrgID:      org.ID,
		CustomerID: leaver,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("want pgx.ErrNoRows after Leave, got %v", err)
	}
}

func TestLeaveLastAdmin(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	svc := orgs.NewService(db.PgRO, db.PgW, nil)
	ctx := context.Background()

	soleAdmin := seedCustomer(t, ctx, write, "soleadmin")
	member := seedCustomer(t, ctx, write, "member")
	org, err := svc.CreateOrgWithDefaults(ctx, soleAdmin, "last-admin")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	mustAddMember(t, ctx, write, org.ID, member, orgsv1.OrgRole_ORG_ROLE_MEMBER.String())

	if err := svc.Leave(ctx, org.ID, soleAdmin); !errors.Is(err, orgs.ErrLastAdmin) {
		t.Fatalf("want ErrLastAdmin, got %v", err)
	}
}

func TestLeaveSoloAdminReturnsLastAdmin(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	// CreateOrgWithDefaults seats the caller as ADMIN. When that admin is also
	// the sole member, the admin-count guard fires first, so the caller sees
	// ErrLastAdmin (the more actionable error: "promote someone first").
	// ErrLastMember is only reachable for non-admin sole members; see
	// TestLeaveNonAdminSoleMember for that path.
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	svc := orgs.NewService(db.PgRO, db.PgW, nil)
	ctx := context.Background()

	solo := seedCustomer(t, ctx, write, "solo")
	org, err := svc.CreateOrgWithDefaults(ctx, solo, "only-member")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := svc.Leave(ctx, org.ID, solo); !errors.Is(err, orgs.ErrLastAdmin) {
		t.Fatalf("want ErrLastAdmin, got %v", err)
	}
}

func TestLeaveNotMember(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	svc := orgs.NewService(db.PgRO, db.PgW, nil)
	ctx := context.Background()

	owner := seedCustomer(t, ctx, write, "owner")
	stranger := seedCustomer(t, ctx, write, "stranger")
	org, err := svc.CreateOrgWithDefaults(ctx, owner, "not-member")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := svc.Leave(ctx, org.ID, stranger); !errors.Is(err, orgs.ErrMemberNotFound) {
		t.Fatalf("want ErrMemberNotFound, got %v", err)
	}
}

func TestUpdateMemberRolePromote(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	svc := orgs.NewService(db.PgRO, db.PgW, nil)
	ctx := context.Background()

	admin := seedCustomer(t, ctx, write, "admin")
	member := seedCustomer(t, ctx, write, "member")
	org, err := svc.CreateOrgWithDefaults(ctx, admin, "promote-test")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	mustAddMember(t, ctx, write, org.ID, member, orgsv1.OrgRole_ORG_ROLE_MEMBER.String())

	got, err := svc.UpdateMemberRole(ctx, org.ID, member, orgs.RoleAdmin)
	if err != nil {
		t.Fatalf("UpdateMemberRole: %v", err)
	}
	if got.Role != orgs.RoleAdmin.String() {
		t.Fatalf("want role=ADMIN, got %q", got.Role)
	}
}

func TestUpdateMemberRoleRejectsDemote(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	svc := orgs.NewService(db.PgRO, db.PgW, nil)
	ctx := context.Background()

	admin := seedCustomer(t, ctx, write, "admin")
	co := seedCustomer(t, ctx, write, "coadmin")
	org, err := svc.CreateOrgWithDefaults(ctx, admin, "demote-test")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	mustAddMember(t, ctx, write, org.ID, co, orgsv1.OrgRole_ORG_ROLE_ADMIN.String())

	if _, err := svc.UpdateMemberRole(ctx, org.ID, co, orgs.RoleMember); !errors.Is(err, orgs.ErrUnsupportedRoleTransition) {
		t.Fatalf("want ErrUnsupportedRoleTransition for demote, got %v", err)
	}
}

// TestUpdateMemberRoleRejectsNoOpMemberToMember and
// TestUpdateMemberRoleRejectsNoOpAdminToAdmin pin the *unallowed* same-role
// "no-op" transitions. Together with the promote/demote tests they cover the
// full 2×2 {current,new} ∈ {MEMBER,ADMIN} matrix — a regression that
// accidentally permits any transition other than MEMBER→ADMIN would fail
// one of these four cases regardless of how the guard is written.
func TestUpdateMemberRoleRejectsNoOpMemberToMember(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	svc := orgs.NewService(db.PgRO, db.PgW, nil)
	ctx := context.Background()

	admin := seedCustomer(t, ctx, write, "admin")
	member := seedCustomer(t, ctx, write, "member")
	org, err := svc.CreateOrgWithDefaults(ctx, admin, "noop-mm")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	mustAddMember(t, ctx, write, org.ID, member, orgsv1.OrgRole_ORG_ROLE_MEMBER.String())

	if _, err := svc.UpdateMemberRole(ctx, org.ID, member, orgs.RoleMember); !errors.Is(err, orgs.ErrUnsupportedRoleTransition) {
		t.Fatalf("want ErrUnsupportedRoleTransition for MEMBER->MEMBER, got %v", err)
	}
}

func TestUpdateMemberRoleRejectsNoOpAdminToAdmin(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	svc := orgs.NewService(db.PgRO, db.PgW, nil)
	ctx := context.Background()

	admin := seedCustomer(t, ctx, write, "admin")
	coadmin := seedCustomer(t, ctx, write, "coadmin")
	org, err := svc.CreateOrgWithDefaults(ctx, admin, "noop-aa")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	mustAddMember(t, ctx, write, org.ID, coadmin, orgsv1.OrgRole_ORG_ROLE_ADMIN.String())

	if _, err := svc.UpdateMemberRole(ctx, org.ID, coadmin, orgs.RoleAdmin); !errors.Is(err, orgs.ErrUnsupportedRoleTransition) {
		t.Fatalf("want ErrUnsupportedRoleTransition for ADMIN->ADMIN, got %v", err)
	}
}

func TestUpdateMemberRoleNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	svc := orgs.NewService(db.PgRO, db.PgW, nil)
	ctx := context.Background()

	admin := seedCustomer(t, ctx, write, "admin")
	stranger := seedCustomer(t, ctx, write, "stranger")
	org, err := svc.CreateOrgWithDefaults(ctx, admin, "notfound-test")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := svc.UpdateMemberRole(ctx, org.ID, stranger, orgs.RoleAdmin); !errors.Is(err, orgs.ErrMemberNotFound) {
		t.Fatalf("want ErrMemberNotFound, got %v", err)
	}
}

func TestLeaveNonAdminSoleMember(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	svc := orgs.NewService(db.PgRO, db.PgW, nil)
	ctx := context.Background()

	// Construct an unusual but defensively-supported state: a non-admin sole member.
	// The public API cannot produce this — CreateOrgWithDefaults seats the caller as
	// ADMIN, and Leave/RemoveMember/UpdateMemberRole all preserve the invariant of
	// at least one admin per org. We construct it by seeding an admin + a member,
	// then directly deleting the admin via the unchecked DeleteOrgMember query.
	admin := seedCustomer(t, ctx, write, "admin")
	loner := seedCustomer(t, ctx, write, "loner")
	org, err := svc.CreateOrgWithDefaults(ctx, admin, "non-admin-sole")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	mustAddMember(t, ctx, write, org.ID, loner, orgsv1.OrgRole_ORG_ROLE_MEMBER.String())
	if _, err := write.DeleteOrgMember(ctx, dbwrite.DeleteOrgMemberParams{
		OrgID:      org.ID,
		CustomerID: admin,
	}); err != nil {
		t.Fatalf("force-remove admin: %v", err)
	}

	if err := svc.Leave(ctx, org.ID, loner); !errors.Is(err, orgs.ErrLastMember) {
		t.Fatalf("want ErrLastMember for non-admin sole member, got %v", err)
	}
}

// failingPublisher returns an error from every Publish so we can exercise
// the fire-and-forget silent-drop path in InviteMember without involving NATS.
type failingPublisher struct{}

func (failingPublisher) Publish(_ context.Context, _ string, _ []byte) error {
	return errors.New("simulated publish failure")
}

// TestInviteMemberCountsPublishFailure pins the alarm contract for
// emails.publish_failure_total. If publishing the invite-email job fails:
//
//   - InviteMember must NOT return an error to the caller (fire-and-forget).
//   - The invitation row must remain in PG (tx committed before publish).
//   - The counter must tick with kind="org_member_invite".
//
// Regressions in any of these would silently drop user-visible invite emails
// without any operator-facing signal. The counter is the ONLY mechanism
// surfacing the drop, so we assert it explicitly.
func TestInviteMemberCountsPublishFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	reader := sdkmetric.NewManualReader()
	prevProvider := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prevProvider) })

	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	svc := orgs.NewService(db.PgRO, db.PgW, failingPublisher{})
	ctx := context.Background()

	inviter := seedCustomer(t, ctx, write, "inviter")
	org, err := svc.CreateOrgWithDefaults(ctx, inviter, "publish-failure")
	if err != nil {
		t.Fatalf("seed org: %v", err)
	}

	dispatch, err := svc.InviteMember(ctx, org.ID, inviter, "invitee@example.com")
	if err != nil {
		t.Fatalf("InviteMember should swallow publish failure, got: %v", err)
	}
	if dispatch.Invitation.ID == "" {
		t.Fatal("expected invitation to be created")
	}

	// Confirm the invitation row really is persisted (tx committed pre-publish).
	var n int
	if err := db.PgW.QueryRow(ctx, "select count(*) from org_invitations where id = $1", dispatch.Invitation.ID).Scan(&n); err != nil {
		t.Fatalf("scan invitation: %v", err)
	}
	if n != 1 {
		t.Fatalf("want invitation persisted, got count=%d", n)
	}

	assertEmailFailureCounter(t, reader, "github.com/pug-sh/pug/internal/core/orgs", "org_member_invite")
}

// assertEmailFailureCounter is a small helper used by both orgs and auth
// failure-publish tests. It collects from the manual reader and asserts at
// least one tick exists on emails.publish_failure_total with the expected
// {kind} attribute on the named instrumentation scope.
func assertEmailFailureCounter(t *testing.T, reader sdkmetric.Reader, scope, kind string) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("reader.Collect: %v", err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != scope {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name != "emails.publish_failure_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("emails.publish_failure_total: want Sum[int64], got %T", m.Data)
			}
			for _, dp := range sum.DataPoints {
				if got, ok := dp.Attributes.Value("kind"); ok && got.AsString() == kind {
					total += dp.Value
				}
			}
		}
	}
	if total == 0 {
		t.Fatalf("expected emails.publish_failure_total{kind=%q,scope=%q} to be > 0", kind, scope)
	}
}

func TestLeaveTwoAdminsRaceExactlyOneSucceeds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	svc := orgs.NewService(db.PgRO, db.PgW, nil)
	ctx := context.Background()

	// Loop with a fresh org per iteration: a regression where the lock CTE
	// silently allowed both goroutines through could pass a single trial by
	// coincidence of scheduling. Five iterations make that hard.
	const iterations = 5
	for i := 0; i < iterations; i++ {
		a := seedCustomer(t, ctx, write, "racer-a")
		b := seedCustomer(t, ctx, write, "racer-b")
		org, err := svc.CreateOrgWithDefaults(ctx, a, "race-org")
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		mustAddMember(t, ctx, write, org.ID, b, orgsv1.OrgRole_ORG_ROLE_ADMIN.String())

		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			errs[0] = svc.Leave(ctx, org.ID, a)
		}()
		go func() {
			defer wg.Done()
			errs[1] = svc.Leave(ctx, org.ID, b)
		}()
		wg.Wait()

		var successes, lastAdmins int
		for _, err := range errs {
			switch {
			case err == nil:
				successes++
			case errors.Is(err, orgs.ErrLastAdmin):
				lastAdmins++
			default:
				t.Fatalf("iter %d: unexpected error from concurrent Leave: %v", i, err)
			}
		}
		if successes != 1 || lastAdmins != 1 {
			t.Fatalf("iter %d: want exactly 1 success and 1 ErrLastAdmin, got successes=%d lastAdmins=%d", i, successes, lastAdmins)
		}

		// Direct DB post-condition: the org must have exactly one admin and
		// one total member remaining. Catches subtler regressions a query
		// rewrite that dropped the org_id filter could pass the (successes,
		// lastAdmins) tuple by coincidence but mutate state incorrectly.
		var adminCount, memberCount int
		if err := db.PgW.QueryRow(ctx,
			`select count(*) filter (where role = 'ORG_ROLE_ADMIN'), count(*) from org_members where org_id = $1`,
			org.ID,
		).Scan(&adminCount, &memberCount); err != nil {
			t.Fatalf("iter %d: scan post-state: %v", i, err)
		}
		if adminCount != 1 || memberCount != 1 {
			t.Fatalf("iter %d: post-state want adminCount=1 memberCount=1, got admin=%d total=%d", i, adminCount, memberCount)
		}
	}
}
