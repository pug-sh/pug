package auth

import (
	"context"
	"log/slog"
	"net/http"

	coreoauth "github.com/pug-sh/pug/internal/core/auth/oauth"
	"github.com/pug-sh/pug/internal/core/email/secret"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/domainname"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/slogx"
)

// SignInProvider is a provider that can sign a domain in: a config provider, or an
// org's connection when ConnectionID is set.
type SignInProvider struct {
	Config       coreoauth.ProviderConfig
	ConnectionID string
}

type SignInDiscovery struct {
	Domain     string
	Providers  []SignInProvider
	RequireSSO bool
}

func (s *Service) WithSSOConnections(cipher *secret.Cipher, client *http.Client) *Service {
	s.ssoCipher = cipher
	s.oauth.WithConnections(client)
	return s
}

// DiscoverSignIn returns the providers for the email's domain and whether it requires SSO.
// It says nothing about whether an account exists.
func (s *Service) DiscoverSignIn(ctx context.Context, email string) (SignInDiscovery, error) {
	domain := domainname.Of(email)
	if domain == "" {
		return SignInDiscovery{}, nil
	}
	providers, err := s.ProvidersFor(ctx, domain)
	if err != nil {
		return SignInDiscovery{}, err
	}
	required, err := dbread.New(s.pgW).IsSSORequired(ctx, domain)
	if err != nil {
		slog.ErrorContext(ctx, "failed to check whether the domain requires sso", slogx.Error(err), slog.String("domain", domain))
		telemetry.RecordError(ctx, err)
		return SignInDiscovery{}, err
	}
	return SignInDiscovery{Domain: domain, Providers: providers, RequireSSO: required}, nil
}

// ProvidersFor returns the providers that can prove domain: its connection and the
// config providers that list it or, when there are none, Google. A failed connection
// lookup still returns the config providers, so a refusal can show them.
func (s *Service) ProvidersFor(ctx context.Context, domain string) ([]SignInProvider, error) {
	var out []SignInProvider
	var err error
	if s.ssoCipher != nil {
		var conn coreorgs.SSOConnection
		var ok bool
		conn, ok, err = coreorgs.SSOConnectionForDomain(ctx, dbread.New(s.pgW), domain)
		if ok {
			out = append(out, SignInProvider{
				ConnectionID: conn.ID,
				Config: coreoauth.ProviderConfig{
					Type:        coreoauth.ProviderTypeOIDC,
					DisplayName: conn.Label,
					ClientID:    conn.ClientID,
					IssuerURL:   conn.IssuerURL,
					Scopes:      coreoauth.DefaultScopes,
				},
			})
		}
	}
	providers := s.oauthCfg.ProvidersFor(domain)
	if len(out) > 0 {
		providers = s.oauthCfg.ListedFor(domain)
	}
	for _, p := range providers {
		out = append(out, SignInProvider{Config: p})
	}
	return out, err
}
