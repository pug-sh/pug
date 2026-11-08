package server

import (
	"testing"

	"github.com/pug-sh/pug/internal/app/server/mcp"
)

func TestMCPIssuer(t *testing.T) {
	for raw, want := range map[string]string{
		"":                          "",
		" \n":                       "",
		"https://api.pugs.dev":      "https://api.pugs.dev",
		"https://api.pugs.dev/":     "https://api.pugs.dev",
		" https://api.pugs.dev ":    "https://api.pugs.dev",
		"https://api.pugs.dev:8443": "https://api.pugs.dev:8443",
		"http://localhost:3000":     "http://localhost:3000",
		"http://127.0.0.1:3000":     "http://127.0.0.1:3000",
	} {
		if got, err := mcpIssuer(raw); err != nil || got != want {
			t.Errorf("mcpIssuer(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{
		"api.pugs.dev",
		"/mcp",
		"https://",
		"https:///",
		"https:",
		"http://api.pugs.dev",
		"http://[::1]:3000",
		"ftp://api.pugs.dev",
		"https://api.pugs.dev/api",
		"https://api.pugs.dev//",
		"https://API.pugs.dev",
		"HTTPS://api.pugs.dev",
		"https://user@api.pugs.dev",
		"https://api.pugs.dev?x=1",
		"https://api.pugs.dev?",
		"https://api.pugs.dev#top",
		"https://api.pugs.dev#",
		"https://api pugs.dev",
	} {
		if got, err := mcpIssuer(raw); err == nil {
			t.Errorf("mcpIssuer(%q) = %q, want an error", raw, got)
		}
	}
}

func TestNewMCPOAuth(t *testing.T) {
	t.Setenv("PUG_API_BASE_URL", "")
	t.Setenv("PUG_DASHBOARD_BASE_URL", "")
	if oauth, err := newMCPOAuth(t.Context(), nil); oauth != (mcp.OAuth{}) || err != nil {
		t.Errorf("unset: newMCPOAuth = %v, %v; want OAuth off", oauth, err)
	}

	t.Setenv("PUG_API_BASE_URL", "https://api.pugs.dev")
	for _, dashboard := range []string{"", "/", "app.pug.sh", "http://app.pug.sh"} {
		t.Setenv("PUG_DASHBOARD_BASE_URL", dashboard)
		if _, err := newMCPOAuth(t.Context(), nil); err == nil {
			t.Errorf("PUG_DASHBOARD_BASE_URL=%q was accepted", dashboard)
		}
	}
	t.Setenv("PUG_DASHBOARD_BASE_URL", "https://app.pug.sh/")
	oauth, err := newMCPOAuth(t.Context(), nil)
	if err != nil || oauth.Grants == nil || oauth.Grants.Issuer() != "https://api.pugs.dev" ||
		oauth.AuthorizationEndpoint != "https://app.pug.sh/oauth/authorize" {
		t.Errorf("newMCPOAuth = %+v, %v; want OAuth on with the consent page at https://app.pug.sh/oauth/authorize", oauth, err)
	}
	t.Setenv("PUG_API_BASE_URL", "http://localhost:3000")
	t.Setenv("PUG_DASHBOARD_BASE_URL", "http://localhost:5173")
	if _, err := newMCPOAuth(t.Context(), nil); err != nil {
		t.Errorf("pug dev's localhost URLs were refused: %v", err)
	}
	t.Setenv("PUG_API_BASE_URL", "https://api.pugs.dev/v1")
	if _, err := newMCPOAuth(t.Context(), nil); err == nil {
		t.Error("a PUG_API_BASE_URL with a path was accepted")
	}
}
