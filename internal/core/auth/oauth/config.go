package oauth

import (
	"context"
	"strings"

	"github.com/sethvargo/go-envconfig"
)

// Config holds OAuth provider credentials and redirect URI policy.
type Config struct {
	GoogleClientID         string `env:"PUG_OAUTH_GOOGLE_CLIENT_ID"`
	GoogleClientSecret     string `env:"PUG_OAUTH_GOOGLE_CLIENT_SECRET"`
	RedirectURIAllowlist   string `env:"PUG_OAUTH_REDIRECT_URI_ALLOWLIST"`
	redirectURIAllowlist   []string
}

func LoadConfig(ctx context.Context) (Config, error) {
	var cfg Config
	if err := envconfig.Process(ctx, &cfg); err != nil {
		return Config{}, err
	}
	cfg.redirectURIAllowlist = parseAllowlist(cfg.RedirectURIAllowlist)
	return cfg, nil
}

func parseAllowlist(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (c Config) IsProviderEnabled(name ProviderName) bool {
	switch name {
	case ProviderGoogle:
		return c.GoogleClientID != "" && c.GoogleClientSecret != ""
	default:
		return false
	}
}

func (c Config) AllowedRedirectURI(uri string) bool {
	if len(c.redirectURIAllowlist) == 0 {
		return false
	}
	for _, allowed := range c.redirectURIAllowlist {
		if uri == allowed {
			return true
		}
	}
	return false
}

// TestConfig builds OAuth config for unit tests with an explicit redirect allowlist.
func TestConfig(clientID, clientSecret string, allowlist ...string) Config {
	return Config{
		GoogleClientID:       clientID,
		GoogleClientSecret:   clientSecret,
		redirectURIAllowlist: allowlist,
	}
}
