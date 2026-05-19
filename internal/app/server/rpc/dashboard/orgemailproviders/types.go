package orgemailproviders

import (
	"errors"
	"fmt"

	"github.com/pug-sh/pug/internal/core/email"
	orgemailprovidersv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/orgemailproviders/v1"
)

// protoKindToCore maps the proto enum to the core/email ProviderKind string.
// Returns an error for UNSPECIFIED or any unknown kind.
func protoKindToCore(k orgemailprovidersv1.OrgEmailProviderKind) (email.ProviderKind, error) {
	switch k {
	case orgemailprovidersv1.OrgEmailProviderKind_ORG_EMAIL_PROVIDER_KIND_SMTP:
		return email.ProviderKindSMTP, nil
	case orgemailprovidersv1.OrgEmailProviderKind_ORG_EMAIL_PROVIDER_KIND_RESEND:
		return email.ProviderKindResend, nil
	default:
		return "", fmt.Errorf("unknown provider kind %s", k)
	}
}

// coreKindToProto maps a core/email ProviderKind back to the proto enum.
// Unknown kinds map to UNSPECIFIED.
func coreKindToProto(k email.ProviderKind) orgemailprovidersv1.OrgEmailProviderKind {
	switch k {
	case email.ProviderKindSMTP:
		return orgemailprovidersv1.OrgEmailProviderKind_ORG_EMAIL_PROVIDER_KIND_SMTP
	case email.ProviderKindResend:
		return orgemailprovidersv1.OrgEmailProviderKind_ORG_EMAIL_PROVIDER_KIND_RESEND
	default:
		return orgemailprovidersv1.OrgEmailProviderKind_ORG_EMAIL_PROVIDER_KIND_UNSPECIFIED
	}
}

// configFromSetRequest extracts the oneof config from a SetRequest into a
// (kind, cfg) pair suitable for email.EncodeProviderConfig. Returns an error
// if the oneof is unset.
func configFromSetRequest(req *orgemailprovidersv1.SetRequest) (email.ProviderKind, any, error) {
	switch c := req.Config.(type) {
	case *orgemailprovidersv1.SetRequest_Smtp:
		smtp := c.Smtp
		return email.ProviderKindSMTP, email.SMTPConfig{
			Host:     smtp.GetHost(),
			Port:     int(smtp.GetPort()),
			Username: smtp.GetUsername(),
			Password: smtp.GetPassword(),
			UseTLS:   smtp.GetUseTls(),
		}, nil
	case *orgemailprovidersv1.SetRequest_Resend:
		return email.ProviderKindResend, email.ResendConfig{APIKey: c.Resend.GetApiKey()}, nil
	default:
		return "", nil, errors.New("config oneof is required")
	}
}

// redactPlaintext returns a display string that exposes shape but no secrets.
// For Resend the API key is shown as prefix+***+suffix. For SMTP the
// host/port/username are shown but the password is replaced with ***.
func redactPlaintext(kind email.ProviderKind, plaintext []byte) (string, error) {
	switch kind {
	case email.ProviderKindResend:
		cfg, err := email.DecodeResendConfig(plaintext)
		if err != nil {
			return "", err
		}
		return redactAPIKey(cfg.APIKey), nil
	case email.ProviderKindSMTP:
		cfg, err := email.DecodeSMTPConfig(plaintext)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("smtp://%s:***@%s:%d", cfg.Username, cfg.Host, cfg.Port), nil
	default:
		return "", fmt.Errorf("unknown kind %q", kind)
	}
}

// redactAPIKey reveals the first 8 and last 4 characters of an API key, with
// "***" in between. Short keys (<= 12 chars) collapse to "***" entirely so we
// never reveal a meaningful prefix or suffix of a key that is too short to
// safely redact.
func redactAPIKey(apiKey string) string {
	if len(apiKey) <= 12 {
		return "***"
	}
	return apiKey[:8] + "***" + apiKey[len(apiKey)-4:]
}
