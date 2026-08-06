package oauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/coreos/go-oidc/v3/oidc"
)

type registryTestProvider struct{ name ProviderName }

func (p registryTestProvider) Name() ProviderName { return p.name }
func (registryTestProvider) VerifyCredential(context.Context, string) (*Identity, error) {
	return nil, ErrInvalidCredential
}

func TestRegistryLookup(t *testing.T) {
	registry := NewRegistry(registryTestProvider{name: "company_sso"})
	provider, err := registry.Get("company_sso")
	if err != nil || provider.Name() != "company_sso" {
		t.Fatalf("provider = %v, err = %v", provider, err)
	}
	if _, err := registry.Get("missing"); !errors.Is(err, ErrOAuthProviderDisabled) {
		t.Fatalf("err = %v, want ErrOAuthProviderDisabled", err)
	}
}

func TestNewRegistryFromConfigRejectsUnknownProviderType(t *testing.T) {
	_, err := NewRegistryFromConfig(context.Background(), Config{Providers: []ProviderConfig{{
		ID: "unknown", Type: ProviderType("unknown"),
	}}})
	if !errors.Is(err, ErrOAuthProviderDisabled) {
		t.Fatalf("err = %v, want ErrOAuthProviderDisabled", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func discoveryContext(issuer string) context.Context {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := fmt.Sprintf(`{"issuer":%q,"authorization_endpoint":%q,"token_endpoint":%q,"jwks_uri":%q}`,
			issuer, issuer+"/authorize", issuer+"/token", issuer+"/keys")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})}
	return oidc.ClientContext(context.Background(), client)
}

func TestNewRegistryFromConfigBuildsConfiguredProviders(t *testing.T) {
	tests := []struct {
		name     string
		issuer   string
		provider ProviderConfig
	}{
		{
			name:   "google via oidc",
			issuer: "https://accounts.google.com",
			provider: ProviderConfig{
				ID: "google", Type: ProviderTypeOIDC, ClientID: "google-client", IssuerURL: "https://accounts.google.com",
			},
		},
		{
			name:   "oidc",
			issuer: "https://login.example.com/realms/main",
			provider: ProviderConfig{
				ID: "company_sso", Type: ProviderTypeOIDC, ClientID: "pug", IssuerURL: "https://login.example.com/realms/main",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry, err := NewRegistryFromConfig(discoveryContext(tt.issuer), Config{Providers: []ProviderConfig{tt.provider}})
			if err != nil {
				t.Fatal(err)
			}
			provider, err := registry.Get(ProviderName(tt.provider.ID))
			if err != nil || provider.Name() != ProviderName(tt.provider.ID) {
				t.Fatalf("provider = %v, err = %v", provider, err)
			}
		})
	}
}

func TestLoadConfigWithoutProviders(t *testing.T) {
	t.Setenv("PUG_CONFIG_FILE", "")
	cfg, err := LoadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers) != 0 {
		t.Fatalf("providers = %v", cfg.Providers)
	}
}
