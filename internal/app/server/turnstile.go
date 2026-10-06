package server

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/pug-sh/pug/internal/deps/turnstile"
	"github.com/sethvargo/go-envconfig"
)

// newTurnstile returns nil when neither key is set, which turns the check off.
func newTurnstile(ctx context.Context) (*turnstile.Verifier, error) {
	var cfg turnstile.Config
	if err := envconfig.Process(ctx, &cfg); err != nil {
		return nil, err
	}
	cfg.SiteKey, cfg.SecretKey = strings.TrimSpace(cfg.SiteKey), strings.TrimSpace(cfg.SecretKey)
	if (cfg.SiteKey == "") != (cfg.SecretKey == "") {
		return nil, errors.New("set both PUG_TURNSTILE_SITE_KEY and PUG_TURNSTILE_SECRET_KEY, or neither")
	}
	slog.InfoContext(ctx, "turnstile", slog.Bool("enabled", cfg.SiteKey != ""))
	if cfg.SiteKey == "" {
		return nil, nil
	}
	return turnstile.New(cfg), nil
}
