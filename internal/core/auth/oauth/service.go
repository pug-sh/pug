package oauth

import (
	"context"
	"errors"
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

func (s *Service) VerifyIdentity(ctx context.Context, provider ProviderName, credential string) (*Identity, error) {
	if !s.cfg.IsProviderEnabled(provider) {
		return nil, ErrOAuthProviderDisabled
	}

	p, err := s.registry.Get(provider)
	if err != nil {
		return nil, err
	}

	ident, err := p.VerifyCredential(ctx, credential)
	if err != nil {
		// Client-input outcomes pass through unchanged so the handler can map
		// them precisely; the handler keeps the client-facing message vague.
		if errors.Is(err, ErrInvalidCredential) || errors.Is(err, ErrUnverifiedEmail) {
			return nil, err
		}
		// An unexpected verifier error (network, malformed provider response) is
		// recorded at this detect site and collapsed to ErrInvalidCredential so
		// provider internals never reach the client.
		slog.ErrorContext(ctx, "oauth credential verification failed", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return nil, ErrInvalidCredential
	}

	return ident, nil
}
