package ses

import (
	"context"
	"errors"
	"fmt"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
	"github.com/aws/smithy-go"
	emailspec "github.com/pug-sh/pug/internal/core/email/spec"
)

type Config struct {
	Region string `env:"PUG_SES_REGION"`
}

type emailSender interface {
	SendEmail(context.Context, *sesv2.SendEmailInput, ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error)
}

type Provider struct {
	client emailSender
}

func New(ctx context.Context, cfg Config) (*Provider, error) {
	if cfg.Region == "" {
		return nil, errors.New("ses: region is required (set PUG_SES_REGION)")
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("ses: load AWS config: %w", err)
	}

	return &Provider{client: sesv2.NewFromConfig(awsCfg)}, nil
}

// sanitizeHeader strips CR and LF from header values. The SES SDK likely
// rejects malformed headers internally, but stripping here keeps parity with
// the SMTP provider's defense-in-depth (smtp.go::sanitizeHeader) — any
// user-controlled string that reaches Subject or Reply-To has already been
// scrubbed once at the application layer, this is the second pass.
func sanitizeHeader(v string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(v)
}

func (p *Provider) Send(ctx context.Context, msg emailspec.Message) error {
	from := sanitizeHeader(msg.From)
	subject := sanitizeHeader(msg.Subject)
	replyTo := sanitizeHeader(msg.ReplyTo)
	input := &sesv2.SendEmailInput{
		FromEmailAddress: &from,
		Destination: &types.Destination{
			ToAddresses: []string{sanitizeHeader(msg.To)},
		},
		Content: &types.EmailContent{
			Simple: &types.Message{
				Subject: &types.Content{
					Data: &subject,
				},
				Body: &types.Body{
					Html: &types.Content{
						Data: &msg.HTMLBody,
					},
					Text: &types.Content{
						Data: &msg.TextBody,
					},
				},
			},
		},
	}
	if replyTo != "" {
		input.ReplyToAddresses = []string{replyTo}
	}

	sent, err := p.client.SendEmail(ctx, input)
	if err != nil {
		wrappedErr := fmt.Errorf("ses send email: %w", err)
		if isPermanentSendError(err) {
			return emailspec.NewPermanentError(wrappedErr)
		}
		return wrappedErr
	}
	if sent == nil || sent.MessageId == nil || *sent.MessageId == "" {
		return emailspec.NewPermanentError(fmt.Errorf("ses send email: empty response"))
	}
	return nil
}

func isPermanentSendError(err error) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}

	switch apiErr.ErrorCode() {
	case "BadRequestException", "MailFromDomainNotVerifiedException", "MessageRejected", "NotFoundException":
		return true
	default:
		return false
	}
}
