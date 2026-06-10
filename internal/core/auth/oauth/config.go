package oauth

import (
	"context"

	"github.com/sethvargo/go-envconfig"
)

// Config holds OAuth provider credentials.
type Config struct {
	GoogleClientID string `env:"PUG_OAUTH_GOOGLE_CLIENT_ID"`
}

func LoadConfig(ctx context.Context) (Config, error) {
	var cfg Config
	if err := envconfig.Process(ctx, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) IsProviderEnabled(name ProviderName) bool {
	switch name {
	case ProviderGoogle:
		return c.GoogleClientID != ""
	default:
		return false
	}
}

// TestConfig builds OAuth config for unit tests.
func TestConfig(clientID string) Config {
	return Config{GoogleClientID: clientID}
}
