package email_test

import (
	"strings"
	"testing"

	coreemail "github.com/pug-sh/pug/internal/core/email"
)

func TestEncodeDecodeSMTPConfig(t *testing.T) {
	cfg := coreemail.SMTPConfig{
		Host:     "smtp.example.com",
		Port:     587,
		Username: "ops@example.com",
		Password: "shh",
		UseTLS:   true,
	}
	raw, err := coreemail.EncodeProviderConfig(coreemail.ProviderKindSMTP, cfg)
	if err != nil {
		t.Fatalf("EncodeProviderConfig: %v", err)
	}
	if !strings.Contains(string(raw), "shh") {
		t.Fatalf("expected plaintext password in JSON, got %s", raw)
	}
	got, err := coreemail.DecodeSMTPConfig(raw)
	if err != nil {
		t.Fatalf("DecodeSMTPConfig: %v", err)
	}
	if got != cfg {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", got, cfg)
	}
}

func TestEncodeDecodeResendConfig(t *testing.T) {
	cfg := coreemail.ResendConfig{APIKey: "sk_test_abcdef1234"}
	raw, err := coreemail.EncodeProviderConfig(coreemail.ProviderKindResend, cfg)
	if err != nil {
		t.Fatalf("EncodeProviderConfig: %v", err)
	}
	got, err := coreemail.DecodeResendConfig(raw)
	if err != nil {
		t.Fatalf("DecodeResendConfig: %v", err)
	}
	if got != cfg {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", got, cfg)
	}
}

func TestEncodeProviderConfigRejectsKindMismatch(t *testing.T) {
	if _, err := coreemail.EncodeProviderConfig(coreemail.ProviderKindResend, coreemail.SMTPConfig{}); err == nil {
		t.Fatal("expected error when kind doesn't match struct type")
	}
}
