package ses

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	sesv2types "github.com/aws/aws-sdk-go-v2/service/sesv2/types"
	emailspec "github.com/pug-sh/pug/internal/core/email/spec"
)

func TestProviderSendBuildsSESRequest(t *testing.T) {
	fake := &fakeSender{
		output: &sesv2.SendEmailOutput{
			MessageId: stringPtr("message-123"),
		},
	}
	provider := &Provider{client: fake}

	err := provider.Send(context.Background(), emailspec.Message{
		From:     "noreply@example.com",
		ReplyTo:  "support@example.com",
		To:       "user@example.com",
		Subject:  "Verify your email",
		HTMLBody: "<p>Hello</p>",
		TextBody: "Hello",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if fake.input == nil {
		t.Fatal("expected SendEmail input")
	}
	if got := *fake.input.FromEmailAddress; got != "noreply@example.com" {
		t.Fatalf("FromEmailAddress = %q", got)
	}
	if got := fake.input.Destination.ToAddresses; len(got) != 1 || got[0] != "user@example.com" {
		t.Fatalf("ToAddresses = %#v", got)
	}
	if got := fake.input.ReplyToAddresses; len(got) != 1 || got[0] != "support@example.com" {
		t.Fatalf("ReplyToAddresses = %#v", got)
	}
	if got := *fake.input.Content.Simple.Subject.Data; got != "Verify your email" {
		t.Fatalf("Subject.Data = %q", got)
	}
	if got := *fake.input.Content.Simple.Body.Html.Data; got != "<p>Hello</p>" {
		t.Fatalf("Body.Html.Data = %q", got)
	}
	if got := *fake.input.Content.Simple.Body.Text.Data; got != "Hello" {
		t.Fatalf("Body.Text.Data = %q", got)
	}
}

// TestProviderSendEmptyMessageIDIsPermanent pins the response-validation
// branch at ses.go:75. SES returning a 200 with an absent or empty MessageId
// is anomalous; treating it as transient would cause the worker to retry
// indefinitely on a class of broken responses that the next attempt won't fix.
func TestProviderSendEmptyMessageIDIsPermanent(t *testing.T) {
	cases := []struct {
		name   string
		output *sesv2.SendEmailOutput
	}{
		{"nil_message_id", &sesv2.SendEmailOutput{MessageId: nil}},
		{"empty_string_message_id", &sesv2.SendEmailOutput{MessageId: stringPtr("")}},
		{"nil_output", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := &Provider{client: &fakeSender{output: tc.output}}
			err := provider.Send(context.Background(), emailspec.Message{
				From: "noreply@example.com", To: "user@example.com",
				Subject: "x", HTMLBody: "<p>x</p>", TextBody: "x",
			})
			if err == nil {
				t.Fatal("expected error on empty response")
			}
			if !emailspec.IsPermanentError(err) {
				t.Fatalf("expected permanent error so worker DLQs, got %v", err)
			}
		})
	}
}

func TestProviderSendWrapsPermanentErrors(t *testing.T) {
	provider := &Provider{
		client: &fakeSender{
			err: &sesv2types.MessageRejected{Message: stringPtr("rejected")},
		},
	}

	err := provider.Send(context.Background(), emailspec.Message{
		From:     "noreply@example.com",
		To:       "user@example.com",
		Subject:  "Verify your email",
		HTMLBody: "<p>Hello</p>",
		TextBody: "Hello",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !emailspec.IsPermanentError(err) {
		t.Fatalf("expected permanent error, got %T: %v", err, err)
	}
}

func TestProviderSendKeepsTransientErrorsRetryable(t *testing.T) {
	provider := &Provider{
		client: &fakeSender{
			err: errors.New("timeout"),
		},
	}

	err := provider.Send(context.Background(), emailspec.Message{
		From:     "noreply@example.com",
		To:       "user@example.com",
		Subject:  "Verify your email",
		HTMLBody: "<p>Hello</p>",
		TextBody: "Hello",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if emailspec.IsPermanentError(err) {
		t.Fatalf("expected retryable error, got %T: %v", err, err)
	}
}

func TestNewLoadsAWSConfig(t *testing.T) {
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "test-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret-key")

	provider, err := New(context.Background(), Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if provider == nil || provider.client == nil {
		t.Fatal("expected provider client")
	}
}

type fakeSender struct {
	input  *sesv2.SendEmailInput
	output *sesv2.SendEmailOutput
	err    error
}

func (f *fakeSender) SendEmail(_ context.Context, input *sesv2.SendEmailInput, _ ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error) {
	f.input = input
	return f.output, f.err
}

func stringPtr(v string) *string {
	return &v
}
