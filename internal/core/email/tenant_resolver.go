package email

import (
	"context"
	"fmt"

	"github.com/pug-sh/pug/internal/core/email/secret"
	resenddeps "github.com/pug-sh/pug/internal/deps/email/resend"
	smtpdeps "github.com/pug-sh/pug/internal/deps/email/smtp"
)

// providerRepo is what TenantAwareResolver needs from OrgProviderRepo. Kept
// narrow so tests can inject a stub without standing up Redis.
type providerRepo interface {
	Get(ctx context.Context, orgID string) (CachedProviderEntry, error)
}

type TenantAwareResolver struct {
	repo            providerRepo
	cipher          *secret.Cipher
	fallback        Provider
	operatorFrom    string
	operatorReplyTo string
}

func NewTenantAwareResolver(repo providerRepo, cipher *secret.Cipher, fallback Provider, operatorFrom, operatorReplyTo string) *TenantAwareResolver {
	return &TenantAwareResolver{
		repo:            repo,
		cipher:          cipher,
		fallback:        fallback,
		operatorFrom:    operatorFrom,
		operatorReplyTo: operatorReplyTo,
	}
}

func (r *TenantAwareResolver) Resolve(ctx context.Context, tenantID *string) (Provider, ResolvedFrom, error) {
	if tenantID == nil {
		return r.fallback, ResolvedFrom{From: r.operatorFrom, ReplyTo: r.operatorReplyTo}, nil
	}
	entry, err := r.repo.Get(ctx, *tenantID)
	if err != nil {
		return nil, ResolvedFrom{}, err
	}
	if !entry.Present {
		return r.fallback, ResolvedFrom{From: r.operatorFrom, ReplyTo: r.operatorReplyTo}, nil
	}

	plaintext, err := r.cipher.Decrypt(entry.SecretCiphertext)
	if err != nil {
		return nil, ResolvedFrom{}, fmt.Errorf("decrypt org %s email provider: %w", *tenantID, err)
	}

	provider, err := buildProvider(ProviderKind(entry.Kind), plaintext)
	if err != nil {
		return nil, ResolvedFrom{}, err
	}
	return provider, ResolvedFrom{From: entry.FromAddress, ReplyTo: entry.ReplyTo}, nil
}

func buildProvider(kind ProviderKind, plaintext []byte) (Provider, error) {
	switch kind {
	case ProviderKindResend:
		cfg, err := DecodeResendConfig(plaintext)
		if err != nil {
			return nil, err
		}
		return resenddeps.New(resenddeps.Config{APIKey: cfg.APIKey})
	case ProviderKindSMTP:
		cfg, err := DecodeSMTPConfig(plaintext)
		if err != nil {
			return nil, err
		}
		return smtpdeps.New(smtpdeps.Config{
			Host:     cfg.Host,
			Port:     cfg.Port,
			Username: cfg.Username,
			Password: cfg.Password,
			UseTLS:   cfg.UseTLS,
		})
	default:
		return nil, fmt.Errorf("email: unknown provider kind %q", kind)
	}
}
