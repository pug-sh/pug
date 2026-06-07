package oauth

import (
	"context"
	"errors"
	"log/slog"

	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/slogx"
)

// Service verifies IdP credentials from the dashboard.
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
		if errors.Is(err, ErrInvalidCredential) {
			return nil, err
		}
		slog.ErrorContext(ctx, "oauth credential verification failed", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return nil, ErrInvalidCredential
	}

	return ident, nil
}
