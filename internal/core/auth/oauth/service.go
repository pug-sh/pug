package oauth

import (
	"context"
	"errors"
	"log/slog"

	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/slogx"
	"golang.org/x/oauth2"
)

// Service orchestrates OAuth begin/complete flows.
type Service struct {
	cfg      Config
	registry *Registry
	state    *StateStore
}

func NewService(cfg Config, registry *Registry, state *StateStore) *Service {
	return &Service{
		cfg:      cfg,
		registry: registry,
		state:    state,
	}
}

type BeginResult struct {
	AuthorizationURL string
	State            string
}

func (s *Service) Begin(ctx context.Context, provider ProviderName, redirectURI string) (BeginResult, error) {
	if !s.cfg.IsProviderEnabled(provider) {
		return BeginResult{}, ErrOAuthProviderDisabled
	}
	if !s.cfg.AllowedRedirectURI(redirectURI) {
		return BeginResult{}, ErrInvalidRedirectURI
	}

	p, err := s.registry.Get(provider)
	if err != nil {
		return BeginResult{}, err
	}

	state, err := NewStateToken()
	if err != nil {
		slog.ErrorContext(ctx, "failed to generate oauth state", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return BeginResult{}, err
	}

	codeVerifier := oauth2.GenerateVerifier()
	codeChallenge := oauth2.S256ChallengeFromVerifier(codeVerifier)

	if err := s.state.Save(ctx, provider, redirectURI, codeVerifier, state); err != nil {
		slog.ErrorContext(ctx, "failed to save oauth state", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return BeginResult{}, err
	}

	return BeginResult{
		AuthorizationURL: p.AuthorizationURL(state, redirectURI, codeChallenge),
		State:            state,
	}, nil
}

// ExchangeIdentity consumes OAuth state, exchanges the authorization code, and validates the IdP identity.
func (s *Service) ExchangeIdentity(ctx context.Context, provider ProviderName, code, stateToken string) (*Identity, error) {
	if !s.cfg.IsProviderEnabled(provider) {
		return nil, ErrOAuthProviderDisabled
	}

	stored, err := s.state.Consume(ctx, stateToken)
	if err != nil {
		if errors.Is(err, ErrInvalidState) {
			return nil, ErrInvalidState
		}
		slog.ErrorContext(ctx, "failed to consume oauth state", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return nil, err
	}
	if ProviderName(stored.Provider) != provider {
		return nil, ErrInvalidState
	}

	p, err := s.registry.Get(provider)
	if err != nil {
		return nil, err
	}

	ident, err := p.Exchange(ctx, stored.RedirectURI, code, stored.CodeVerifier)
	if err != nil {
		if errors.Is(err, ErrOAuthExchangeFailed) || errors.Is(err, ErrOAuthExchangeInvalid) {
			return nil, err
		}
		slog.ErrorContext(ctx, "oauth token exchange failed", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return nil, ErrOAuthExchangeFailed
	}

	return ident, nil
}
