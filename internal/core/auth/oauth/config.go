package oauth

import (
	"context"
	"slices"

	appconfig "github.com/pug-sh/pug/internal/config"
)

type Config appconfig.AuthConfig
type ProviderConfig = appconfig.AuthProvider
type ProviderType = appconfig.ProviderType

const (
	ProviderTypeOIDC = appconfig.ProviderTypeOIDC
)

func LoadConfig(ctx context.Context) (Config, error) {
	cfg, err := appconfig.Load(ctx)
	if err != nil {
		return Config{}, err
	}
	return Config(cfg.Auth), nil
}

func (c Config) IsProviderEnabled(name ProviderName) bool {
	for _, provider := range c.Providers {
		if ProviderName(provider.ID) == name {
			return true
		}
	}
	return false
}

// ProvidersFor returns the providers that can prove domain: those whose emailDomains
// list it or, when none does, Google.
func (c Config) ProvidersFor(domain string) []ProviderConfig {
	if listed := c.ListedFor(domain); len(listed) > 0 {
		return listed
	}
	var google []ProviderConfig
	for _, p := range c.Providers {
		if appconfig.IsGoogleIssuer(p.IssuerURL) {
			google = append(google, p)
		}
	}
	return google
}

func (c Config) ListedFor(domain string) []ProviderConfig {
	var out []ProviderConfig
	for _, p := range c.Providers {
		if slices.Contains(p.EmailDomains, domain) {
			out = append(out, p)
		}
	}
	return out
}

// TestConfig builds OAuth config for unit tests.
func TestConfig(clientID string) Config {
	if clientID == "" {
		return Config{}
	}
	return Config{Providers: []ProviderConfig{{
		ID:          "test_oidc",
		Type:        ProviderTypeOIDC,
		DisplayName: "Test OIDC",
		ClientID:    clientID,
		IssuerURL:   "https://idp.example.com",
		Scopes:      []string{"openid", "profile", "email"},
	}}}
}
