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

func TestLoadPreservesServerOnlyClientSecret(t *testing.T) {
	cfg, err := loadFile(t, `{"version":1,"auth":{"providers":[{"id":"google","type":"oidc","displayName":"Google","clientId":"client","clientSecret":"server-only-secret","issuerUrl":"https://accounts.google.com"}]}}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Auth.Providers[0].ClientSecret; got != "server-only-secret" {
		t.Fatalf("clientSecret = %q", got)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	_, err := loadFile(t, `{"version":1,"auth":{"providers":[]},"surprise":true}`)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("err = %v, want unknown field", err)
	}
}

func TestLoadRejectsInvalidProviderConfiguration(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"unsupported version", `{"version":2,"auth":{"providers":[]}}`, "version must be 1"},
		{"multiple JSON values", `{"version":1,"auth":{"providers":[]}} {}`, "multiple JSON values"},
		{"invalid id", `{"version":1,"auth":{"providers":[{"id":"Company SSO","type":"oidc","displayName":"Company SSO","clientId":"client","issuerUrl":"https://login.example.com"}]}}`, "id must match"},
		{"duplicate id", `{"version":1,"auth":{"providers":[{"id":"same","type":"oidc","displayName":"First","clientId":"client","issuerUrl":"https://first.example.com"},{"id":"same","type":"oidc","displayName":"Second","clientId":"client","issuerUrl":"https://second.example.com"}]}}`, "is duplicated"},
		{"blank display name", `{"version":1,"auth":{"providers":[{"id":"sso","type":"oidc","displayName":"   ","clientId":"client","issuerUrl":"https://login.example.com"}]}}`, "displayName must contain"},
		{"missing client id", `{"version":1,"auth":{"providers":[{"id":"sso","type":"oidc","displayName":"SSO","clientId":"   ","issuerUrl":"https://login.example.com"}]}}`, "clientId is required"},
		{"missing issuer", `{"version":1,"auth":{"providers":[{"id":"sso","type":"oidc","displayName":"SSO","clientId":"client"}]}}`, "issuerUrl"},
		{"unsupported type", `{"version":1,"auth":{"providers":[{"id":"custom","type":"saml","displayName":"SAML","clientId":"client"}]}}`, "type must be"},
		{"issuer credentials", `{"version":1,"auth":{"providers":[{"id":"sso","type":"oidc","displayName":"SSO","clientId":"client","issuerUrl":"https://user:pass@login.example.com"}]}}`, "without credentials"},
		{"issuer query", `{"version":1,"auth":{"providers":[{"id":"sso","type":"oidc","displayName":"SSO","clientId":"client","issuerUrl":"https://login.example.com?realm=main"}]}}`, "without credentials"},
		{"scope with whitespace", `{"version":1,"auth":{"providers":[{"id":"sso","type":"oidc","displayName":"SSO","clientId":"client","issuerUrl":"https://login.example.com","scopes":["openid","bad scope"]}]}}`, "one non-empty scope"},
		{"missing openid scope", `{"version":1,"auth":{"providers":[{"id":"sso","type":"oidc","displayName":"SSO","clientId":"client","issuerUrl":"https://login.example.com","scopes":["profile","email"]}]}}`, "must include openid"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadFile(t, tt.body)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestLoadAcceptsLocalHTTPAndNormalizesScopes(t *testing.T) {
	cfg, err := loadFile(t, `{"version":1,"auth":{"providers":[{"id":"local_sso","type":"oidc","displayName":" Local SSO ","clientId":" pug ","issuerUrl":" http://localhost:8080/realms/pug ","scopes":[" openid "," profile "]}]}}`)
	if err != nil {
		t.Fatal(err)
	}
	provider := cfg.Auth.Providers[0]
	if provider.DisplayName != "Local SSO" || provider.ClientID != "pug" || provider.IssuerURL != "http://localhost:8080/realms/pug" {
		t.Fatalf("provider was not normalized: %+v", provider)
	}
	if strings.Join(provider.Scopes, " ") != "openid profile" {
		t.Fatalf("scopes = %v", provider.Scopes)
	}
}

func TestLoadRejectsInsecureRemoteIssuer(t *testing.T) {
	_, err := loadFile(t, `{"version":1,"auth":{"providers":[{"id":"sso","type":"oidc","displayName":"SSO","clientId":"pug","issuerUrl":"http://login.example.com"}]}}`)
	if err == nil || !strings.Contains(err.Error(), "must use HTTPS") {
		t.Fatalf("err = %v, want HTTPS validation", err)
	}
}

func TestLoadWithoutExternalProviders(t *testing.T) {
	t.Setenv("PUG_CONFIG_FILE", "")
	cfg, err := config.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != config.Version || len(cfg.Auth.Providers) != 0 {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestLoadReportsMissingConfigFile(t *testing.T) {
	t.Setenv("PUG_CONFIG_FILE", filepath.Join(t.TempDir(), "missing.json"))
	_, err := config.Load(context.Background())
	if err == nil || !strings.Contains(err.Error(), "open") {
		t.Fatalf("err = %v, want open error", err)
	}
}
