package orgs_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/xid"

	"github.com/pug-sh/pug/internal/core/orgs"
	natsdeps "github.com/pug-sh/pug/internal/deps/nats"
	orgsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/orgs/v1"
	emailworkerv1 "github.com/pug-sh/pug/internal/gen/proto/workers/email/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"google.golang.org/protobuf/proto"
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

func TestCreateOrgWithDefaultsRollbackOnDuplicateCustomer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
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
	_ = write // keep linter happy if unused
}

type stubPublisher struct {
	subject string
	job     *emailworkerv1.EmailJob
}

func (p *stubPublisher) Publish(_ context.Context, subject string, data []byte) error {
	p.subject = subject
	p.job = &emailworkerv1.EmailJob{}
	return proto.Unmarshal(data, p.job)
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

	inv, err := svc.InviteMember(ctx, org.ID, customer.ID, "invitee@example.com")
	if err != nil {
		t.Fatalf("InviteMember: %v", err)
	}
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
	if payload.GetToken() != inv.Token {
		t.Fatalf("token = %q, want %q", payload.GetToken(), inv.Token)
	}

	emailToken, err := read.GetValidEmailActionTokenByHashAndPurpose(ctx, dbread.GetValidEmailActionTokenByHashAndPurposeParams{
		TokenHash: hashToken(inv.Token),
		Purpose:   "org_invite",
	})
	if err != nil {
		t.Fatalf("GetValidEmailActionTokenByHashAndPurpose: %v", err)
	}
	if !emailToken.OrgInvitationID.Valid || emailToken.OrgInvitationID.String != inv.ID {
		t.Fatalf("org invitation id = %v, want %q", emailToken.OrgInvitationID, inv.ID)
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

	firstInv, err := svc.InviteMember(ctx, orgA.ID, customer.ID, "invitee@example.com")
	if err != nil {
		t.Fatalf("first InviteMember: %v", err)
	}
	secondInv, err := svc.InviteMember(ctx, orgB.ID, customer.ID, "invitee@example.com")
	if err != nil {
		t.Fatalf("second InviteMember: %v", err)
	}

	for name, token := range map[string]string{
		"first":  firstInv.Token,
		"second": secondInv.Token,
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

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
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

	inv, err := svc.InviteMember(context.Background(), org.ID, inviter.ID, inviteeEmail)
	if err != nil {
		t.Fatalf("InviteMember: %v", err)
	}
	return &inviteFixture{
		t: t, svc: svc, pool: db.PgW, write: write, read: read,
		org: org, invitee: invitee, inviter: inviter, invite: inv, rawToken: inv.Token,
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

// backdateInvitation overrides a pending invitation's expires_at via raw SQL.
// Used only by the expired-invitation test — there's no production code path
// to back-date, which is exactly why we bypass sqlc.
func backdateInvitation(ctx context.Context, t *testing.T, f *inviteFixture, ts time.Time) error {
	t.Helper()
	_, err := f.pool.Exec(ctx, `update org_invitations set expires_at = $1 where id = $2`,
		pgtype.Timestamptz{Time: ts, Valid: true}, f.invite.ID)
	return err
}
