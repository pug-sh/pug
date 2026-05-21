package email

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"buf.build/go/protovalidate"
	"github.com/jackc/pgx/v5"

	coreemail "github.com/pug-sh/pug/internal/core/email"
	natsworker "github.com/pug-sh/pug/internal/deps/nats"
	emailworkerv1 "github.com/pug-sh/pug/internal/gen/proto/workers/email/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"google.golang.org/protobuf/proto"
)

type Processor struct {
	read   *dbread.Queries
	mailer *coreemail.Service
}

func NewProcessor(read *dbread.Queries, mailer *coreemail.Service) *Processor {
	return &Processor{read: read, mailer: mailer}
}

func (p *Processor) ProcessMessage(ctx context.Context, data []byte) error {
	job := &emailworkerv1.EmailJob{}
	if err := proto.Unmarshal(data, job); err != nil {
		return natsworker.NewPermanentError(err).With("worker", "misc_email")
	}
	if err := protovalidate.Validate(job); err != nil {
		return natsworker.NewPermanentError(err).With("worker", "misc_email")
	}

	var err error
	switch payload := job.Payload.(type) {
	case *emailworkerv1.EmailJob_SignupVerifyWelcome:
		err = p.mailer.SendSignupVerifyWelcome(
			ctx,
			payload.SignupVerifyWelcome.GetEmail(),
			payload.SignupVerifyWelcome.GetToken(),
			idempotencyKeyForJob(job),
		)
	case *emailworkerv1.EmailJob_PasswordReset:
		err = p.mailer.SendPasswordReset(
			ctx,
			payload.PasswordReset.GetEmail(),
			payload.PasswordReset.GetToken(),
			idempotencyKeyForJob(job),
		)
	case *emailworkerv1.EmailJob_VerificationResend:
		err = p.mailer.SendVerificationResend(
			ctx,
			payload.VerificationResend.GetEmail(),
			payload.VerificationResend.GetToken(),
			idempotencyKeyForJob(job),
		)
	case *emailworkerv1.EmailJob_MagicLink:
		err = p.mailer.SendMagicLink(
			ctx,
			payload.MagicLink.GetEmail(),
			payload.MagicLink.GetToken(),
			idempotencyKeyForJob(job),
		)
	case *emailworkerv1.EmailJob_OrgMemberInvite:
		details, lookupErr := p.read.GetOrgInvitationEmailContextByID(ctx, payload.OrgMemberInvite.GetInvitationId())
		if lookupErr != nil {
			if errors.Is(lookupErr, pgx.ErrNoRows) {
				return natsworker.NewPermanentError(lookupErr).
					With("worker", "misc_email").
					With("invitation_id", payload.OrgMemberInvite.GetInvitationId())
			}
			return lookupErr
		}
		err = p.mailer.SendOrgMemberInvite(
			ctx,
			details.OrgID,
			payload.OrgMemberInvite.GetEmail(),
			details.OrgDisplayName,
			details.InviterDisplayName,
			payload.OrgMemberInvite.GetToken(),
			idempotencyKeyForJob(job),
		)
	default:
		return natsworker.NewPermanentError(fmt.Errorf("unknown email job payload %T", job.Payload)).
			With("worker", "misc_email")
	}

	if err != nil {
		if coreemail.IsPermanentError(err) {
			return natsworker.NewPermanentError(err).With("worker", "misc_email")
		}
		return err
	}
	return nil
}

func idempotencyKeyForJob(job *emailworkerv1.EmailJob) string {
	switch payload := job.Payload.(type) {
	case *emailworkerv1.EmailJob_SignupVerifyWelcome:
		return "signup_verify_welcome:" + strings.TrimSpace(payload.SignupVerifyWelcome.GetToken())
	case *emailworkerv1.EmailJob_PasswordReset:
		return "password_reset:" + strings.TrimSpace(payload.PasswordReset.GetToken())
	case *emailworkerv1.EmailJob_VerificationResend:
		return "verification_resend:" + strings.TrimSpace(payload.VerificationResend.GetToken())
	case *emailworkerv1.EmailJob_MagicLink:
		return "magic_link:" + strings.TrimSpace(payload.MagicLink.GetToken())
	case *emailworkerv1.EmailJob_OrgMemberInvite:
		return "org_member_invite:" + strings.TrimSpace(payload.OrgMemberInvite.GetInvitationId())
	default:
		return ""
	}
}
