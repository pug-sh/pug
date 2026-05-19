package email

import (
	"encoding/json"
	"fmt"
)

type ProviderKind string

const (
	ProviderKindSMTP   ProviderKind = "ORG_EMAIL_PROVIDER_KIND_SMTP"
	ProviderKindResend ProviderKind = "ORG_EMAIL_PROVIDER_KIND_RESEND"
)

// Valid reports whether k is a known provider kind. Use this to gate any
// path that takes a ProviderKind from outside the closed set above (database
// rows, proto enum mapping, etc.).
func (k ProviderKind) Valid() bool {
	switch k {
	case ProviderKindSMTP, ProviderKindResend:
		return true
	}
	return false
}

type SMTPConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	UseTLS   bool   `json:"use_tls"`
}

type ResendConfig struct {
	APIKey string `json:"api_key"`
}

func EncodeProviderConfig(kind ProviderKind, cfg any) ([]byte, error) {
	switch kind {
	case ProviderKindSMTP:
		if _, ok := cfg.(SMTPConfig); !ok {
			return nil, fmt.Errorf("email: kind %s requires SMTPConfig, got %T", kind, cfg)
		}
	case ProviderKindResend:
		if _, ok := cfg.(ResendConfig); !ok {
			return nil, fmt.Errorf("email: kind %s requires ResendConfig, got %T", kind, cfg)
		}
	default:
		return nil, fmt.Errorf("email: unknown provider kind %q", kind)
	}
	return json.Marshal(cfg)
}

func DecodeSMTPConfig(raw []byte) (SMTPConfig, error) {
	var cfg SMTPConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return SMTPConfig{}, fmt.Errorf("email: decode smtp config: %w", err)
	}
	return cfg, nil
}

func DecodeResendConfig(raw []byte) (ResendConfig, error) {
	var cfg ResendConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return ResendConfig{}, fmt.Errorf("email: decode resend config: %w", err)
	}
	return cfg, nil
}
