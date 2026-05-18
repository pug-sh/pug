package orgs_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/pug-sh/pug/internal/core/orgs"
	natsdeps "github.com/pug-sh/pug/internal/deps/nats"
	emailworkerv1 "github.com/pug-sh/pug/internal/gen/proto/workers/email/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"google.golang.org/protobuf/proto"
)

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
