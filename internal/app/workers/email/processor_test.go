package email

import (
	"context"
	"errors"
	"strings"
	"testing"

	coreemail "github.com/pug-sh/pug/internal/core/email"
	natsworker "github.com/pug-sh/pug/internal/deps/nats"
	"github.com/pug-sh/pug/internal/deps/postgres"
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

func TestProcessorSignupPayloadMapsToVerifyLink(t *testing.T) {
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
		Payload: &emailworkerv1.EmailJob_SignupVerifyWelcome{
			SignupVerifyWelcome: &emailworkerv1.SignUpVerifyWelcomePayload{
				Email: proto.String("test@example.com"),
				Token: proto.String("verify-token"),
			},
		},
	})
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}

	if err := processor.ProcessMessage(context.Background(), data); err != nil {
		t.Fatalf("ProcessMessage: %v", err)
	}
	if len(provider.msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(provider.msgs))
	}
	if !strings.Contains(provider.msgs[0].TextBody, "https://dashboard.example/verify-email?token=verify-token") {
		t.Fatalf("expected verify link in text body, got %q", provider.msgs[0].TextBody)
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
		Payload: &emailworkerv1.EmailJob_PasswordReset{
			PasswordReset: &emailworkerv1.PasswordResetPayload{
				Email: proto.String("test@example.com"),
				Token: proto.String("reset-token"),
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
	if !strings.Contains(provider.msgs[0].TextBody, "Inviter invited you to join Worker Org") {
		t.Fatalf("unexpected invite body: %q", provider.msgs[0].TextBody)
	}
	if !strings.Contains(provider.msgs[0].TextBody, "https://dashboard.example/accept-invite?token=invite-token") {
		t.Fatalf("unexpected invite link: %q", provider.msgs[0].TextBody)
	}
}
