package config_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pug-sh/pug/internal/config"
)

func loadFile(t *testing.T, body string) (config.Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pug.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PUG_CONFIG_FILE", path)
	t.Setenv("PUG_OAUTH_GOOGLE_CLIENT_ID", "")
	return config.Load(context.Background())
}

func TestLoadOIDCProviderDefaultsScopesAndPreservesIssuer(t *testing.T) {
	cfg, err := loadFile(t, `{
  "version": 1,
  "auth": {"providers": [{
    "id": "company_sso",
    "type": "oidc",
    "displayName": "Company SSO",
    "clientId": "pug",
    "issuerUrl": "https://login.example.com/realms/main/"
  }]}
}`)
	if err != nil {
		t.Fatal(err)
	}
	provider := cfg.Auth.Providers[0]
	if provider.IssuerURL != "https://login.example.com/realms/main/" {
		t.Fatalf("issuer = %q", provider.IssuerURL)
	}
	if strings.Join(provider.Scopes, " ") != "openid profile email" {
		t.Fatalf("scopes = %v", provider.Scopes)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	_, err := loadFile(t, `{"version":1,"auth":{"providers":[]},"surprise":true}`)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("err = %v, want unknown field", err)
	}
}

func TestLoadRejectsInsecureRemoteIssuer(t *testing.T) {
	_, err := loadFile(t, `{"version":1,"auth":{"providers":[{"id":"sso","type":"oidc","displayName":"SSO","clientId":"pug","issuerUrl":"http://login.example.com"}]}}`)
	if err == nil || !strings.Contains(err.Error(), "must use HTTPS") {
		t.Fatalf("err = %v, want HTTPS validation", err)
	}
}

func TestLoadRejectsAmbiguousLegacyGoogleConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pug.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"auth":{"providers":[]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PUG_CONFIG_FILE", path)
	t.Setenv("PUG_OAUTH_GOOGLE_CLIENT_ID", "legacy-client")
	_, err := config.Load(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cannot be used together") {
		t.Fatalf("err = %v, want ambiguity error", err)
	}
}

func TestLoadLegacyGoogleEnvironment(t *testing.T) {
	t.Setenv("PUG_CONFIG_FILE", "")
	t.Setenv("PUG_OAUTH_GOOGLE_CLIENT_ID", "legacy-client")
	cfg, err := config.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	provider := cfg.Auth.Providers[0]
	if provider.ID != "google" || provider.Type != config.ProviderTypeGoogle || provider.ClientID != "legacy-client" {
		t.Fatalf("provider = %+v", provider)
	}
}
