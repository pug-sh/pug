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

	key, err := idempotencyKeyForJob(job)
	if err != nil {
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
