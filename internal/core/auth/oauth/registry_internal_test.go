package oauth

import (
	"context"
	"errors"
	"testing"
)

type registryTestProvider struct{ name ProviderName }

func (p registryTestProvider) Name() ProviderName { return p.name }

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
	_, err := NewRegistryFromConfig(Config{Providers: []ProviderConfig{{
		ID: "unknown", Type: ProviderType("unknown"),
	}}}, DefaultHTTPClient())
	if err == nil || !containsAll(err.Error(), "unsupported type", "unknown") {
		t.Fatalf("err = %v, want the provider id and its bad type named", err)
	}
}

// Building the registry must not touch the network: an IdP that is down at boot
// would otherwise take the whole server with it.
func TestNewRegistryFromConfigBuildsConfiguredProvidersWithoutDiscovery(t *testing.T) {
	for _, provider := range []ProviderConfig{
		{ID: "google", Type: ProviderTypeOIDC, ClientID: "google-client", IssuerURL: "https://accounts.google.com"},
		{ID: "company_sso", Type: ProviderTypeOIDC, ClientID: "pug", IssuerURL: "https://login.example.com/realms/main"},
		{ID: "dead_sso", Type: ProviderTypeOIDC, ClientID: "pug", IssuerURL: "https://127.0.0.1:1/realms/main"},
	} {
		t.Run(provider.ID, func(t *testing.T) {
			registry, err := NewRegistryFromConfig(Config{Providers: []ProviderConfig{provider}}, DefaultHTTPClient())
			if err != nil {
				t.Fatal(err)
			}
			got, err := registry.Get(ProviderName(provider.ID))
			if err != nil || got.Name() != ProviderName(provider.ID) {
				t.Fatalf("provider = %v, err = %v", got, err)
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
