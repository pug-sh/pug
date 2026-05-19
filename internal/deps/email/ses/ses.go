package ses

import (
	"context"
	"errors"
	"fmt"

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
	loadOptions := make([]func(*awsconfig.LoadOptions) error, 0, 1)
	if cfg.Region != "" {
		loadOptions = append(loadOptions, awsconfig.WithRegion(cfg.Region))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return nil, fmt.Errorf("ses: load AWS config: %w", err)
	}

	return &Provider{client: sesv2.NewFromConfig(awsCfg)}, nil
}

func (p *Provider) Send(ctx context.Context, msg emailspec.Message) error {
	input := &sesv2.SendEmailInput{
		FromEmailAddress: &msg.From,
		Destination: &types.Destination{
			ToAddresses: []string{msg.To},
		},
		Content: &types.EmailContent{
			Simple: &types.Message{
				Subject: &types.Content{
					Data: &msg.Subject,
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
	if msg.ReplyTo != "" {
		input.ReplyToAddresses = []string{msg.ReplyTo}
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
