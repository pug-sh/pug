package auth

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pug-sh/pug/internal/apperr"
	coreauth "github.com/pug-sh/pug/internal/core/auth"
	coreoauth "github.com/pug-sh/pug/internal/core/auth/oauth"
	natsdeps "github.com/pug-sh/pug/internal/deps/nats"
	authv1 "github.com/pug-sh/pug/internal/gen/proto/public/auth/v1"
)

// authService is the coreauth.Service surface the handler depends on, defined
// consumer-side so handlers can be unit-tested with a fake (instead of
// re-implementing the mapping logic in tests).
type authService interface {
	SignInWithEmail(ctx context.Context, email, password string) (string, error)
	RequestMagicLink(ctx context.Context, email string) error
	CompleteMagicLink(ctx context.Context, token, reportingTimezone string) (string, error)
	CompleteOAuthSignIn(ctx context.Context, provider coreoauth.ProviderName, credential string) (string, error)
}

type server struct {
	service authService
}

func NewServer(ctx context.Context, pgRO *pgxpool.Pool, pgW *pgxpool.Pool, jwtKey []byte, publisher *natsdeps.NATSClient) (*server, error) {
	oauthCfg, err := coreoauth.LoadConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load oauth config: %w", err)
	}
	service, err := coreauth.NewService(ctx, pgRO, pgW, jwtKey, publisher, oauthCfg)
	if err != nil {
		return nil, err
	}

	return &server{
		service: service,
	}, nil
}

func (s *server) SignInWithEmail(
	ctx context.Context,
	req *connect.Request[authv1.SignInWithEmailRequest],
) (*connect.Response[authv1.SignInWithEmailResponse], error) {
	token, err := s.service.SignInWithEmail(ctx, req.Msg.GetEmail(), req.Msg.GetPassword())
	if err != nil {
		if errors.Is(err, coreauth.ErrInvalidCredentials) {
			return nil, apperr.Unauthenticated(apperr.ReasonInvalidCredentials, "invalid credentials")
		}
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	return connect.NewResponse(&authv1.SignInWithEmailResponse{Token: &token}), nil
}

func (s *server) RequestMagicLink(
	ctx context.Context,
	req *connect.Request[authv1.RequestMagicLinkRequest],
) (*connect.Response[authv1.RequestMagicLinkResponse], error) {
	if err := s.service.RequestMagicLink(ctx, req.Msg.GetEmail()); err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	return connect.NewResponse(&authv1.RequestMagicLinkResponse{}), nil
}

func (s *server) CompleteMagicLink(
	ctx context.Context,
	req *connect.Request[authv1.CompleteMagicLinkRequest],
) (*connect.Response[authv1.CompleteMagicLinkResponse], error) {
	token, err := s.service.CompleteMagicLink(ctx, req.Msg.GetToken(), req.Msg.GetTimezone())
	if err != nil {
		if errors.Is(err, coreauth.ErrInvalidToken) {
			return nil, apperr.Invalid(apperr.ReasonInvalidToken, "invalid or expired link")
		}
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	return connect.NewResponse(&authv1.CompleteMagicLinkResponse{Token: &token}), nil
}

func (s *server) CompleteOAuthSignIn(
	ctx context.Context,
	req *connect.Request[authv1.CompleteOAuthSignInRequest],
) (*connect.Response[authv1.CompleteOAuthSignInResponse], error) {
	provider, err := coreoauth.ProviderFromProto(req.Msg.GetProvider())
	if err != nil {
		return nil, apperr.Invalid(apperr.ReasonOAuthProviderDisabled, "oauth provider is not configured")
	}

	token, err := s.service.CompleteOAuthSignIn(ctx, provider, req.Msg.GetCredential())
	if err != nil {
		return nil, mapOAuthHandlerError(err)
	}
	return connect.NewResponse(&authv1.CompleteOAuthSignInResponse{Token: &token}), nil
}

func mapOAuthHandlerError(err error) error {
	switch {
	case errors.Is(err, coreoauth.ErrOAuthProviderDisabled):
		return apperr.Invalid(apperr.ReasonOAuthProviderDisabled, "oauth provider is not configured")
	case errors.Is(err, coreoauth.ErrUnverifiedEmail):
		// Generic reason intentional: no distinct client action for an unverified IdP
		// email (rare edge), so it maps to plain InvalidArgument.
		return apperr.Invalid(apperr.ReasonInvalidArgument, "email not verified by identity provider") // apperr:exempt
	case errors.Is(err, coreoauth.ErrInvalidCredential):
		// A failed/expired credential is an authentication failure, not a
		// malformed request — return Unauthenticated so clients prompt re-auth
		// (mirrors SignInWithEmail). Message stays vague for anti-enumeration.
		return apperr.Unauthenticated(apperr.ReasonOAuthCredentialInvalid, "oauth sign-in failed")
	default:
		return connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
}
