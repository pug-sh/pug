package oauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/slogx"
)

// Service verifies IdP credentials against configured providers and returns a
// verified Identity.
type Service struct {
	cfg      Config
	registry *Registry
}

func NewService(cfg Config, registry *Registry) *Service {
	return &Service{
		cfg:      cfg,
		registry: registry,
	}
}

func (s *Service) ExchangeCode(ctx context.Context, provider ProviderName, code AuthorizationCode) (*Identity, error) {
	if !s.cfg.IsProviderEnabled(provider) {
		return nil, ErrOAuthProviderDisabled
	}

	p, err := s.registry.Get(provider)
	if err != nil {
		return nil, err
	}
	// Registered but unable to exchange a code is our bug, not a disabled provider.
	codeProvider, ok := p.(AuthorizationCodeProvider)
	if !ok {
		err := fmt.Errorf("%w: provider %q cannot exchange authorization codes", ErrIdentityResolutionFailed, provider)
		slog.ErrorContext(ctx, "oauth provider missing code exchange", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return nil, err
	}

	ident, err := codeProvider.ExchangeCode(ctx, code)
	return s.handleIdentityResult(ctx, provider, ident, err)
}

func (s *Service) handleIdentityResult(ctx context.Context, provider ProviderName, ident *Identity, err error) (*Identity, error) {
	if err != nil {
		// Client-input outcomes pass through unchanged so the handler can map
		// them precisely; the handler keeps the client-facing message vague.
		// Logged at Warn because per request they are the caller's fault, but a
		// sustained rate means a misregistered redirect URI or a missing
		// email_verified claim mapping — both otherwise invisible.
		if errors.Is(err, ErrInvalidCredential) || errors.Is(err, ErrUnverifiedEmail) {
			slog.WarnContext(ctx, "oidc sign-in rejected", slog.String("provider", string(provider)), slogx.Error(err))
			return nil, err
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		slog.ErrorContext(ctx, "oauth identity verification failed", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		// Our own bug stays itself (the handler maps it to Internal); everything
		// else collapses so provider internals never reach the client.
		if errors.Is(err, ErrIdentityResolutionFailed) {
			return nil, err
		}
		return nil, ErrProviderUnavailable
	}
	if ident == nil || ident.Provider() != provider {
		err := fmt.Errorf("%w: provider %q returned an identity for %q", ErrIdentityResolutionFailed, provider, identProvider(ident))
		slog.ErrorContext(ctx, "oauth provider returned a mismatched identity", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return nil, err
	}

	return ident, nil
}

func identProvider(ident *Identity) ProviderName {
	if ident == nil {
		return ""
	}
	return ident.Provider()
}
