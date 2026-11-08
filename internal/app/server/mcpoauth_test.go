package server

import "testing"

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

func TestNewMCPGrants(t *testing.T) {
	t.Setenv("PUG_API_BASE_URL", "")
	t.Setenv("PUG_DASHBOARD_BASE_URL", "")
	if svc, err := newMCPGrants(t.Context(), nil); svc != nil || err != nil {
		t.Errorf("unset: newMCPGrants = %v, %v; want OAuth off", svc, err)
	}

	t.Setenv("PUG_API_BASE_URL", "https://api.pugs.dev")
	for _, dashboard := range []string{"", "/", "app.pug.sh"} {
		t.Setenv("PUG_DASHBOARD_BASE_URL", dashboard)
		if _, err := newMCPGrants(t.Context(), nil); err == nil {
			t.Errorf("PUG_DASHBOARD_BASE_URL=%q was accepted", dashboard)
		}
	}
	t.Setenv("PUG_DASHBOARD_BASE_URL", "https://app.pug.sh")
	if svc, err := newMCPGrants(t.Context(), nil); svc == nil || err != nil {
		t.Errorf("newMCPGrants = %v, %v; want OAuth on", svc, err)
	}
	t.Setenv("PUG_API_BASE_URL", "https://api.pugs.dev/v1")
	if _, err := newMCPGrants(t.Context(), nil); err == nil {
		t.Error("a PUG_API_BASE_URL with a path was accepted")
	}
}
