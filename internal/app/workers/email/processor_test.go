package email

import (
	"context"
	"errors"
	"strings"
	"testing"

	coreemail "github.com/pug-sh/pug/internal/core/email"
	natsworker "github.com/pug-sh/pug/internal/deps/nats"
	"github.com/pug-sh/pug/internal/deps/postgres"
	orgsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/orgs/v1"
	emailworkerv1 "github.com/pug-sh/pug/internal/gen/proto/workers/email/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"google.golang.org/protobuf/proto"
)

type fakeProvider struct {
	msgs []coreemail.Message
	err  error
}

func (p *fakeProvider) Send(_ context.Context, msg coreemail.Message) error {
	p.msgs = append(p.msgs, msg)
	return p.err
}

// TestProcessorMagicLinkIdempotencyKeyIsStableOnRetry pins the contract that
// NATS redelivery (or any duplicate ProcessMessage call) produces the SAME
// idempotency key. Resend and other providers dedupe on this header
// server-side, so a regression that derived the key from time.Now() or a
// random nonce would silently break dedup and surface as duplicate emails.
func TestProcessorMagicLinkIdempotencyKeyIsStableOnRetry(t *testing.T) {
	provider := &fakeProvider{}
	mailer, err := coreemail.NewService(coreemail.Config{
		DashboardBaseURL: "https://dashboard.example",
		From:             "noreply@example.com",
	}, provider)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	processor := NewProcessor(nil, mailer)
	data, err := proto.Marshal(&emailworkerv1.EmailJob{
		Payload: &emailworkerv1.EmailJob_MagicLink{
			MagicLink: &emailworkerv1.MagicLinkPayload{
				Email: proto.String("retry@example.com"),
				Token: proto.String("stable-token"),
			},
		},
	})
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := processor.ProcessMessage(context.Background(), data); err != nil {
			t.Fatalf("ProcessMessage attempt %d: %v", i, err)
		}
	}
	if len(provider.msgs) != 2 {
		t.Fatalf("expected 2 sends, got %d", len(provider.msgs))
	}
	if provider.msgs[0].IdempotencyKey == "" {
		t.Fatal("idempotency key must be non-empty")
	}
	if provider.msgs[0].IdempotencyKey != provider.msgs[1].IdempotencyKey {
		t.Fatalf("idempotency key differs across retries: %q vs %q",
			provider.msgs[0].IdempotencyKey, provider.msgs[1].IdempotencyKey)
	}
}

func TestProcessorPermanentProviderErrorMapsToDLQ(t *testing.T) {
	provider := &fakeProvider{err: coreemail.NewPermanentError(errors.New("bad request"))}
	mailer, err := coreemail.NewService(coreemail.Config{
		DashboardBaseURL: "https://dashboard.example",
		From:             "noreply@example.com",
	}, provider)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	processor := NewProcessor(nil, mailer)
	data, err := proto.Marshal(&emailworkerv1.EmailJob{
		Payload: &emailworkerv1.EmailJob_MagicLink{
			MagicLink: &emailworkerv1.MagicLinkPayload{
				Email: proto.String("test@example.com"),
				Token: proto.String("magic-token"),
			},
		},
	})
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}

	err = processor.ProcessMessage(context.Background(), data)
	if err == nil || !natsworker.IsPermanentError(err) {
		t.Fatalf("expected permanent error, got %v", err)
	}
}

// TestProcessorRejectsMalformedProtoAsPermanent pins that a non-proto payload
// (corrupted byte stream on the NATS subject) is classified as permanent so
// the worker terminates it instead of looping until MaxDeliver.
func TestProcessorRejectsMalformedProtoAsPermanent(t *testing.T) {
	processor := NewProcessor(nil, nil)
	err := processor.ProcessMessage(context.Background(), []byte("not-a-proto"))
	if err == nil || !natsworker.IsPermanentError(err) {
		t.Fatalf("expected permanent error for malformed proto, got %v", err)
	}
}

// TestProcessorRejectsEmptyPayloadAsPermanent pins that an EmailJob with no
// payload oneof set is classified as permanent (protovalidate rejects it).
func TestProcessorRejectsEmptyPayloadAsPermanent(t *testing.T) {
	processor := NewProcessor(nil, nil)
	data, err := proto.Marshal(&emailworkerv1.EmailJob{})
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}
	if err := processor.ProcessMessage(context.Background(), data); err == nil || !natsworker.IsPermanentError(err) {
		t.Fatalf("expected permanent error for empty payload, got %v", err)
	}
}

// TestProcessorOrgInviteMissingInvitationIsPermanent pins that an org invite
// pointing at a missing invitation_id is permanent (DLQ immediately) so the
// worker doesn't burn the retry budget on a row that will never appear.
func TestProcessorOrgInviteMissingInvitationIsPermanent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	provider := &fakeProvider{}
	mailer, err := coreemail.NewService(coreemail.Config{
		DashboardBaseURL: "https://dashboard.example",
		From:             "noreply@example.com",
	}, provider)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	processor := NewProcessor(dbread.New(db.PgW), mailer)
	data, err := proto.Marshal(&emailworkerv1.EmailJob{
		Payload: &emailworkerv1.EmailJob_OrgMemberInvite{
			OrgMemberInvite: &emailworkerv1.OrgMemberInvitePayload{
				Email:        proto.String("ghost@example.com"),
				InvitationId: proto.String("does-not-exist"),
				Token:        proto.String("any-token"),
			},
		},
	})
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}
	err = processor.ProcessMessage(context.Background(), data)
	if err == nil || !natsworker.IsPermanentError(err) {
		t.Fatalf("expected permanent error for missing invitation, got %v", err)
	}
	if len(provider.msgs) != 0 {
		t.Fatalf("expected no send when invitation missing, got %d", len(provider.msgs))
	}
}

func TestProcessorOrgInviteLoadsInvitationContext(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	db := testutil.SetupPostgres(t)
	write := dbwrite.New(db.PgW)
	provider := &fakeProvider{}
	mailer, err := coreemail.NewService(coreemail.Config{
		DashboardBaseURL: "https://dashboard.example",
		From:             "noreply@example.com",
	}, provider)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	processor := NewProcessor(dbread.New(db.PgW), mailer)
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
		ID:          "org-worker",
		DisplayName: "Worker Org",
	})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	inv, err := write.CreateOrgInvitation(ctx, dbwrite.CreateOrgInvitationParams{
		Email:     "invitee@example.com",
		ExpiresAt: postgres.NewTimestamptz(customer.CreateTime.Time.AddDate(0, 0, 7)),
		ID:        "invite-1",
		InviterID: postgres.NewOptionalText(customer.ID),
		OrgID:     org.ID,
		Role:      orgsv1.OrgRole_ORG_ROLE_MEMBER.String(),
		Token:     "invite-token",
	})
	if err != nil {
		t.Fatalf("CreateOrgInvitation: %v", err)
	}
	data, err := proto.Marshal(&emailworkerv1.EmailJob{
		Payload: &emailworkerv1.EmailJob_OrgMemberInvite{
			OrgMemberInvite: &emailworkerv1.OrgMemberInvitePayload{
				Email:        proto.String(inv.Email),
				InvitationId: proto.String(inv.ID),
				Token:        proto.String("invite-token"),
			},
		},
	})
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}

	if err := processor.ProcessMessage(ctx, data); err != nil {
		t.Fatalf("ProcessMessage: %v", err)
	}
	if len(provider.msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(provider.msgs))
	}
	if provider.msgs[0].IdempotencyKey != "org_member_invite:invite-1" {
		t.Fatalf("expected org invite idempotency key, got %q", provider.msgs[0].IdempotencyKey)
	}
	if !strings.Contains(provider.msgs[0].TextBody, "Inviter invited you to join Worker Org") {
		t.Fatalf("unexpected invite body: %q", provider.msgs[0].TextBody)
	}
	if !strings.Contains(provider.msgs[0].TextBody, "https://dashboard.example/magic-link?token=invite-token") {
		t.Fatalf("unexpected invite link: %q", provider.msgs[0].TextBody)
	}
}
