package email

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"buf.build/go/protovalidate"
	"github.com/jackc/pgx/v5"

	coreemail "github.com/pug-sh/pug/internal/core/email"
	natsworker "github.com/pug-sh/pug/internal/deps/nats"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	emailworkerv1 "github.com/pug-sh/pug/internal/gen/proto/workers/email/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/slogx"
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
		slog.ErrorContext(ctx, "failed to unmarshal email job", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return natsworker.NewPermanentError(err).With("worker", "misc_email")
	}
	if err := protovalidate.Validate(job); err != nil {
		slog.ErrorContext(ctx, "email job failed validation", slogx.Error(err),
			slog.String("dispatch_id", job.GetDispatchId()))
		telemetry.RecordError(ctx, err)
		return natsworker.NewPermanentError(err).With("worker", "misc_email")
	}

	key, err := idempotencyKeyForJob(job)
	if err != nil {
		slog.ErrorContext(ctx, "email job has no usable idempotency key", slogx.Error(err),
			slog.String("dispatch_id", job.GetDispatchId()))
		telemetry.RecordError(ctx, err)
		return natsworker.NewPermanentError(err).With("worker", "misc_email")
	}

	switch payload := job.Payload.(type) {
	case *emailworkerv1.EmailJob_MagicLink:
		err = p.mailer.SendMagicLink(
			ctx,
			payload.MagicLink.GetEmail(),
			payload.MagicLink.GetToken(),
			key,
		)
	case *emailworkerv1.EmailJob_OrgMemberInvite:
		details, lookupErr := p.read.GetOrgInvitationEmailContextByID(ctx, payload.OrgMemberInvite.GetInvitationId())
		if lookupErr != nil {
			slog.ErrorContext(ctx, "failed to load org invitation email context", slogx.Error(lookupErr),
				slog.String("invitation_id", payload.OrgMemberInvite.GetInvitationId()))
			telemetry.RecordError(ctx, lookupErr)
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
			key,
		)
	default:
		err := fmt.Errorf("unknown email job payload %T", job.Payload)
		slog.ErrorContext(ctx, "email job carries an unknown payload", slogx.Error(err),
			slog.String("dispatch_id", job.GetDispatchId()))
		telemetry.RecordError(ctx, err)
		return natsworker.NewPermanentError(err).With("worker", "misc_email")
	}

	if err != nil {
		// The mailer does not log or record, so a failed send reaches telemetry
		// only here; magic links are a sign-in path.
		slog.ErrorContext(ctx, "failed to send email", slogx.Error(err),
			slog.String("dispatch_id", job.GetDispatchId()))
		telemetry.RecordError(ctx, err)
		if coreemail.IsPermanentError(err) {
			return natsworker.NewPermanentError(err).With("worker", "misc_email")
		}
		return err
	}
	return nil
}

// idempotencyKeyForJob keys the send on the dispatch id: fresh per issuance, so
// a resend never reuses the original's key, and stable across redeliveries.
// Errors on a blank id — providers skip the header, silently disabling dedup.
func idempotencyKeyForJob(job *emailworkerv1.EmailJob) (string, error) {
	id := strings.TrimSpace(job.GetDispatchId())
	if id == "" {
		return "", errors.New("email job has a blank dispatch id")
	}
	return id, nil
}
