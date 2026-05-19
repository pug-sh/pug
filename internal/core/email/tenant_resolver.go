package email

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/pug-sh/pug/internal/core/email/secret"
	resenddeps "github.com/pug-sh/pug/internal/deps/email/resend"
	smtpdeps "github.com/pug-sh/pug/internal/deps/email/smtp"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/slogx"
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

func NewTenantAwareResolver(repo providerRepo, cipher *secret.Cipher, fallback Provider, operatorFrom, operatorReplyTo string) (*TenantAwareResolver, error) {
	if repo == nil {
		return nil, fmt.Errorf("email: tenant-aware resolver requires a provider repo")
	}
	if cipher == nil {
		return nil, fmt.Errorf("email: tenant-aware resolver requires a cipher (set PUG_EMAIL_PROVIDER_SECRET_KEY)")
	}
	if fallback == nil {
		return nil, fmt.Errorf("email: tenant-aware resolver requires a fallback provider")
	}
	return &TenantAwareResolver{
		repo:            repo,
		cipher:          cipher,
		fallback:        fallback,
		operatorFrom:    operatorFrom,
		operatorReplyTo: operatorReplyTo,
	}, nil
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
		// Decrypt failure is non-retryable: either the operator rotated
		// PUG_EMAIL_PROVIDER_SECRET_KEY without re-encrypting the row, or
		// the ciphertext is corrupted. Retrying with the same key/row will
		// fail identically, so wrap as permanent so the worker DLQs.
		slog.ErrorContext(ctx, "failed to decrypt org email provider; treating as permanent",
			slogx.Error(err), slog.String("org_id", *tenantID))
		telemetry.RecordError(ctx, err)
		return nil, ResolvedFrom{}, NewPermanentError(fmt.Errorf("decrypt org %s email provider: %w", *tenantID, err))
	}

	provider, err := buildProvider(ProviderKind(entry.Kind), plaintext)
	if err != nil {
		// An unknown kind in the DB or a malformed config is a permanent
		// misconfiguration; the same row will keep failing.
		slog.ErrorContext(ctx, "failed to build org email provider; treating as permanent",
			slogx.Error(err), slog.String("org_id", *tenantID), slog.String("kind", entry.Kind))
		telemetry.RecordError(ctx, err)
		return nil, ResolvedFrom{}, NewPermanentError(err)
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
