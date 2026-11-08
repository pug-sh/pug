package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pug-sh/pug/internal/app/server/mcp"
	"github.com/pug-sh/pug/internal/cimd"
	"github.com/pug-sh/pug/internal/core/mcpgrants"
	"github.com/sethvargo/go-envconfig"
)

type mcpOAuthConfig struct {
	APIBaseURL       string `env:"PUG_API_BASE_URL"`
	DashboardBaseURL string `env:"PUG_DASHBOARD_BASE_URL"`
}

// consentPath is the dashboard page that serves the authorization endpoint.
const consentPath = "/oauth/authorize"

// newMCPOAuth turns OAuth for /mcp on when PUG_API_BASE_URL is set, and returns
// the zero mcp.OAuth when it is off.
func newMCPOAuth(ctx context.Context, pgW *pgxpool.Pool) (mcp.OAuth, error) {
	var cfg mcpOAuthConfig
	if err := envconfig.Process(ctx, &cfg); err != nil {
		return mcp.OAuth{}, err
	}
	issuer, err := mcpIssuer(cfg.APIBaseURL)
	if err != nil {
		return mcp.OAuth{}, err
	}
	slog.InfoContext(ctx, "mcp oauth", slog.Bool("enabled", issuer != ""), slog.String("issuer", issuer))
	if issuer == "" {
		return mcp.OAuth{}, nil
	}
	dashboard := strings.TrimSuffix(cfg.DashboardBaseURL, "/")
	if u, err := url.Parse(dashboard); err != nil || u.Host == "" || !secureOrLocal(u) {
		return mcp.OAuth{}, fmt.Errorf("PUG_API_BASE_URL is set, so PUG_DASHBOARD_BASE_URL must be an https URL, or http on localhost or 127.0.0.1 for development (got %q)", cfg.DashboardBaseURL)
	}
	return mcp.OAuth{
		Grants:                mcpgrants.NewService(pgW, cimd.New(), issuer),
		AuthorizationEndpoint: dashboard + consentPath,
	}, nil
}

// mcpIssuer returns the origin PUG_API_BASE_URL names, or "" when it is unset. The
// metadata sits at the host root and clients compare the issuer byte for byte, so
// it must be a lower-case scheme://host.
func mcpIssuer(raw string) (string, error) {
	origin := strings.TrimSuffix(strings.TrimSpace(raw), "/")
	if origin == "" {
		return "", nil
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || origin != strings.ToLower(u.Scheme+"://"+u.Host) {
		return "", fmt.Errorf("PUG_API_BASE_URL must be an origin such as https://api.example.com, with no path (got %q)", raw)
	}
	if secureOrLocal(u) {
		return origin, nil
	}
	return "", fmt.Errorf("PUG_API_BASE_URL must be https, or http on localhost or 127.0.0.1 for development (got %q)", raw)
}

// secureOrLocal is how OAuth clients want every endpoint served, the consent page
// included: https, or http on localhost or 127.0.0.1 for development.
func secureOrLocal(u *url.URL) bool {
	return u.Scheme == "https" || (u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1"))
}
