package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pug-sh/pug/internal/apperr"
	coreauth "github.com/pug-sh/pug/internal/core/auth"
	coreoauth "github.com/pug-sh/pug/internal/core/auth/oauth"
	natsdeps "github.com/pug-sh/pug/internal/deps/nats"
	authv1 "github.com/pug-sh/pug/internal/gen/proto/public/auth/v1"
	"github.com/pug-sh/pug/internal/slogx"
	"google.golang.org/protobuf/proto"
)

// authService is the coreauth.Service surface the handler depends on, defined
// consumer-side so handlers can be unit-tested with a fake (instead of
// re-implementing the mapping logic in tests).
type authService interface {
	SignInWithEmail(ctx context.Context, email, password string) (coreauth.Session, error)
	RequestMagicLink(ctx context.Context, email string) error
	CompleteMagicLink(ctx context.Context, token, reportingTimezone string) (coreauth.Session, error)
	CompleteOIDCSignIn(ctx context.Context, provider coreoauth.ProviderName, code coreoauth.AuthorizationCode, reportingTimezone string) (coreauth.Session, error)
	RefreshSession(ctx context.Context, refreshToken string) (coreauth.Session, error)
	RevokeSession(ctx context.Context, refreshToken string) error
	DemoSignIn(ctx context.Context) (coreauth.DemoSession, error)
}

type server struct {
	service  authService
	oauthCfg coreoauth.Config
}

func NewServer(ctx context.Context, pgRO *pgxpool.Pool, pgW *pgxpool.Pool, jwtKey []byte, publisher *natsdeps.NATSClient, demoEnabled bool) (*server, error) {
	oauthCfg, err := coreoauth.LoadConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load oauth config: %w", err)
	}
	logExternalProviders(ctx, oauthCfg)

	service, err := coreauth.NewService(ctx, pgRO, pgW, jwtKey, publisher, oauthCfg, demoEnabled)
	if err != nil {
		return nil, err
	}

	return &server{
		service:  service,
		oauthCfg: oauthCfg,
	}, nil
}

// Without this, a half-finished config rollout looks like a healthy boot.
func logExternalProviders(ctx context.Context, cfg coreoauth.Config) {
	ids := make([]string, 0, len(cfg.Providers))
	for _, provider := range cfg.Providers {
		ids = append(ids, provider.ID)
	}
	if len(ids) == 0 {
		msg := "no external sign-in providers configured"
		if os.Getenv(legacyGoogleClientIDVar) != "" {
			msg += "; " + legacyGoogleClientIDVar + " is set but no longer read — move Google into PUG_CONFIG_FILE"
		}
		slog.WarnContext(ctx, msg)
		return
	}
	slog.InfoContext(ctx, "external sign-in providers configured", slog.Any("provider_ids", ids))
}

// Removed in favour of PUG_CONFIG_FILE; kept only to warn operators mid-migration.
const legacyGoogleClientIDVar = "PUG_OAUTH_GOOGLE_CLIENT_ID"

var authProviderTypes = map[coreoauth.ProviderType]authv1.AuthProviderType{
	coreoauth.ProviderTypeOIDC: authv1.AuthProviderType_AUTH_PROVIDER_TYPE_OIDC,
}

func (s *server) GetAuthConfig(
	context.Context,
	*connect.Request[authv1.GetAuthConfigRequest],
) (*connect.Response[authv1.GetAuthConfigResponse], error) {
	providers := make([]*authv1.AuthProviderConfig, 0, len(s.oauthCfg.Providers))
	for _, provider := range s.oauthCfg.Providers {
		providerType, ok := authProviderTypes[provider.Type]
		// A type the browser has no flow for must not render a sign-in button.
		if !ok {
			continue
		}
		providers = append(providers, &authv1.AuthProviderConfig{
			Id:          proto.String(provider.ID),
			Type:        providerType.Enum(),
			DisplayName: proto.String(provider.DisplayName),
			ClientId:    proto.String(provider.ClientID),
			IssuerUrl:   proto.String(provider.IssuerURL),
			Scopes:      provider.Scopes,
		})
	}
	return connect.NewResponse(&authv1.GetAuthConfigResponse{Providers: providers}), nil
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

func (s *server) CompleteOIDCSignIn(
	ctx context.Context,
	req *connect.Request[authv1.CompleteOIDCSignInRequest],
) (*connect.Response[authv1.CompleteOIDCSignInResponse], error) {
	redirectURI, err := validateOIDCRedirectURI(req.Msg.GetRedirectUri(), req.Header().Get("Origin"))
	if err != nil {
		slog.WarnContext(ctx, "rejected oidc redirect uri", slogx.Error(err),
			slog.String("redirect_uri", req.Msg.GetRedirectUri()),
			slog.String("origin", req.Header().Get("Origin")))
		return nil, apperr.Invalid(apperr.ReasonInvalidArgument, "invalid oauth redirect URI") // apperr:exempt
	}

	session, err := s.service.CompleteOIDCSignIn(ctx, coreoauth.ProviderName(req.Msg.GetProviderId()), coreoauth.AuthorizationCode{
		Code:         req.Msg.GetCode(),
		CodeVerifier: req.Msg.GetCodeVerifier(),
		RedirectURI:  redirectURI,
		Nonce:        req.Msg.GetNonce(),
	}, req.Msg.GetTimezone())
	if err != nil {
		return nil, mapOAuthHandlerError(err)
	}
	return connect.NewResponse(&authv1.CompleteOIDCSignInResponse{
		Token:        &session.AccessToken,
		RefreshToken: &session.RefreshToken,
	}), nil
}

func validateOIDCRedirectURI(rawRedirectURI, requestOrigin string) (string, error) {
	redirectURI, err := url.Parse(rawRedirectURI)
	if err != nil || redirectURI.Scheme == "" || redirectURI.Host == "" || redirectURI.User != nil || redirectURI.RawQuery != "" || redirectURI.Fragment != "" {
		return "", errors.New("redirect URI must be absolute and have no credentials, query, or fragment")
	}
	localhost := redirectURI.Hostname() == "localhost" || redirectURI.Hostname() == "127.0.0.1" || redirectURI.Hostname() == "::1"
	if redirectURI.Scheme != "https" && (redirectURI.Scheme != "http" || !localhost) {
		return "", errors.New("redirect URI must use HTTPS except on localhost")
	}
	if redirectURI.Path != "/oauth/callback" {
		return "", errors.New("unexpected callback path")
	}

	if requestOrigin != "" {
		origin, err := url.Parse(requestOrigin)
		if err != nil || origin.Scheme == "" || origin.Host == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
			return "", errors.New("invalid request origin")
		}
		if !sameOrigin(origin, redirectURI) {
			return "", errors.New("request origin does not match redirect URI")
		}
	}
	return redirectURI.String(), nil
}

func sameOrigin(left, right *url.URL) bool {
	return strings.EqualFold(left.Scheme, right.Scheme) &&
		strings.EqualFold(left.Hostname(), right.Hostname()) &&
		effectivePort(left) == effectivePort(right)
}

func effectivePort(value *url.URL) string {
	if port := value.Port(); port != "" {
		return port
	}
	switch strings.ToLower(value.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
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
	case errors.Is(err, coreoauth.ErrProviderUnavailable):
		return apperr.Unavailable(apperr.ReasonOAuthProviderUnavailable, "oauth provider is temporarily unavailable")
	case errors.Is(err, coreoauth.ErrInvalidCredential):
		// A failed/expired credential is an authentication failure, not a
		// malformed request — return Unauthenticated so clients prompt re-auth
		// (mirrors SignInWithEmail). Message stays vague for anti-enumeration.
		return apperr.Unauthenticated(apperr.ReasonOAuthCredentialInvalid, "oauth sign-in failed")
	default:
		return connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}
}
