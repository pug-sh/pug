package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	coreoauth "github.com/pug-sh/pug/internal/core/auth/oauth"
	"github.com/pug-sh/pug/internal/core/email/secret"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/sethvargo/go-envconfig"
)

type ssoConfig struct {
	KeyB64 string `env:"PUG_SSO_SECRET_KEY"`
	// For a self-hosted issuer on an internal network, or a server behind a proxy.
	AllowPrivateIssuers bool `env:"PUG_SSO_ALLOW_PRIVATE_ISSUERS"`
}

// ssoDeps is zero-valued when no key is set, which turns SSO connections off.
type ssoDeps struct {
	cipher *secret.Cipher
	client *http.Client
}

func newSSO(ctx context.Context) (ssoDeps, error) {
	var cfg ssoConfig
	if err := envconfig.Process(ctx, &cfg); err != nil {
		return ssoDeps{}, err
	}
	slog.InfoContext(ctx, "sso connections", slog.Bool("enabled", cfg.KeyB64 != ""),
		slog.Bool("allow_private_issuers", cfg.AllowPrivateIssuers))
	if cfg.KeyB64 == "" {
		return ssoDeps{}, nil
	}
	cipher, err := secret.NewCipher(cfg.KeyB64)
	if err != nil {
		return ssoDeps{}, fmt.Errorf("init sso cipher: %w", err)
	}
	return ssoDeps{cipher: cipher, client: coreoauth.NewConnectionHTTPClient(cfg.AllowPrivateIssuers)}, nil
}

func (d ssoDeps) checkIssuer(ctx context.Context, issuerURL string) error {
	return coreoauth.DiscoverIssuer(ctx, d.client, issuerURL)
}

// warnStrandedSSOConnections reports connections that exist while PUG_SSO_SECRET_KEY is
// empty: their sign-ins fail as a disabled provider, which logs nothing.
func warnStrandedSSOConnections(ctx context.Context, hasConnections func(context.Context) (bool, error)) {
	found, err := hasConnections(ctx)
	if err == nil && found {
		err = errors.New("org SSO connections exist, but PUG_SSO_SECRET_KEY is empty, so none can sign anyone in")
	}
	if err != nil {
		slog.ErrorContext(ctx, "sso connections are off", slogx.Error(err))
		telemetry.RecordError(ctx, err)
	}
}
