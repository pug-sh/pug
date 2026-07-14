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
	SignInWithEmail(ctx context.Context, email, password string) (coreauth.Session, error)
	RequestMagicLink(ctx context.Context, email string) error
	CompleteMagicLink(ctx context.Context, token, reportingTimezone string) (coreauth.Session, error)
	CompleteOAuthSignIn(ctx context.Context, provider coreoauth.ProviderName, credential, reportingTimezone string) (coreauth.Session, error)
	RefreshSession(ctx context.Context, refreshToken string) (coreauth.Session, error)
	RevokeSession(ctx context.Context, refreshToken string) error
	DemoSignIn(ctx context.Context) (coreauth.DemoSession, error)
}

type server struct {
	service authService
}

func NewServer(ctx context.Context, pgRO *pgxpool.Pool, pgW *pgxpool.Pool, jwtKey []byte, publisher *natsdeps.NATSClient, demoEnabled bool) (*server, error) {
	oauthCfg, err := coreoauth.LoadConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load oauth config: %w", err)
	}
	service, err := coreauth.NewService(ctx, pgRO, pgW, jwtKey, publisher, oauthCfg, demoEnabled)
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
	session, err := s.service.SignInWithEmail(ctx, req.Msg.GetEmail(), req.Msg.GetPassword())
	if err != nil {
		if errors.Is(err, coreauth.ErrInvalidCredentials) {
			return nil, apperr.Unauthenticated(apperr.ReasonInvalidCredentials, "invalid credentials")
		}
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	return connect.NewResponse(&authv1.SignInWithEmailResponse{
		Token:        &session.AccessToken,
		RefreshToken: &session.RefreshToken,
	}), nil
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
	session, err := s.service.CompleteMagicLink(ctx, req.Msg.GetToken(), req.Msg.GetTimezone())
	if err != nil {
		if errors.Is(err, coreauth.ErrInvalidToken) {
			return nil, apperr.Invalid(apperr.ReasonInvalidToken, "invalid or expired link")
		}
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	return connect.NewResponse(&authv1.CompleteMagicLinkResponse{
		Token:        &session.AccessToken,
		RefreshToken: &session.RefreshToken,
	}), nil
}

func (s *server) CompleteOAuthSignIn(
	ctx context.Context,
	req *connect.Request[authv1.CompleteOAuthSignInRequest],
) (*connect.Response[authv1.CompleteOAuthSignInResponse], error) {
	provider, err := coreoauth.ProviderFromProto(req.Msg.GetProvider())
	if err != nil {
		return nil, apperr.Invalid(apperr.ReasonOAuthProviderDisabled, "oauth provider is not configured")
	}

	session, err := s.service.CompleteOAuthSignIn(ctx, provider, req.Msg.GetCredential(), req.Msg.GetTimezone())
	if err != nil {
		return nil, mapOAuthHandlerError(err)
	}
	return connect.NewResponse(&authv1.CompleteOAuthSignInResponse{
		Token:        &session.AccessToken,
		RefreshToken: &session.RefreshToken,
	}), nil
}

func (s *server) RefreshSession(
	ctx context.Context,
	req *connect.Request[authv1.RefreshSessionRequest],
) (*connect.Response[authv1.RefreshSessionResponse], error) {
	session, err := s.service.RefreshSession(ctx, req.Msg.GetRefreshToken())
	if err != nil {
		if errors.Is(err, coreauth.ErrInvalidToken) {
			// Refresh failed → the client must sign in again. Unauthenticated (not
			// InvalidArgument) so the FE's existing 401 handling clears the session.
			return nil, apperr.Unauthenticated(apperr.ReasonInvalidToken, "session expired")
		}
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	return connect.NewResponse(&authv1.RefreshSessionResponse{
		Token:        &session.AccessToken,
		RefreshToken: &session.RefreshToken,
	}), nil
}

func (s *server) SignOut(
	ctx context.Context,
	req *connect.Request[authv1.SignOutRequest],
) (*connect.Response[authv1.SignOutResponse], error) {
	if err := s.service.RevokeSession(ctx, req.Msg.GetRefreshToken()); err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	return connect.NewResponse(&authv1.SignOutResponse{}), nil
}

func (s *server) DemoSignIn(
	ctx context.Context,
	_ *connect.Request[authv1.DemoSignInRequest],
) (*connect.Response[authv1.DemoSignInResponse], error) {
	demo, err := s.service.DemoSignIn(ctx)
	if err != nil {
		if errors.Is(err, coreauth.ErrDemoUnavailable) {
			// Disabled (PUG_DEMO_ENABLED off) or the demo account isn't seeded.
			// Unavailable, not Unauthenticated: there are no credentials to be wrong.
			return nil, connect.NewError(connect.CodeUnavailable, errors.New("demo sign-in is not available"))
		}
		return nil, connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
	return connect.NewResponse(&authv1.DemoSignInResponse{
		Token:        &demo.Session.AccessToken,
		RefreshToken: &demo.Session.RefreshToken,
		ProjectId:    &demo.ProjectID,
	}), nil
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
