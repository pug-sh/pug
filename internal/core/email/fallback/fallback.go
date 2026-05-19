// Package fallback constructs the operator-default email provider from
// environment configuration. Shared between the email worker (where it
// supplies the bottom of the per-tenant resolution chain) and the dashboard
// server (where SendTestEmail calls go through the same resolver path).
package fallback

import (
	"context"
	"fmt"

	coreemail "github.com/pug-sh/pug/internal/core/email"
	resenddeps "github.com/pug-sh/pug/internal/deps/email/resend"
	sesdeps "github.com/pug-sh/pug/internal/deps/email/ses"
	"github.com/sethvargo/go-envconfig"
)

type Config struct {
	Name string `env:"PUG_EMAIL_PROVIDER,default=resend"`
}

// NewProvider builds the operator-configured Provider chosen by
// PUG_EMAIL_PROVIDER (resend | ses). Used as the fallback when no per-tenant
// provider is set for an org.
func NewProvider(ctx context.Context) (coreemail.Provider, error) {
	var cfg Config
	if err := envconfig.Process(ctx, &cfg); err != nil {
		return nil, err
	}
	switch cfg.Name {
	case "resend":
		var resendCfg resenddeps.Config
		if err := envconfig.Process(ctx, &resendCfg); err != nil {
			return nil, err
		}
		return resenddeps.New(resendCfg)
	case "ses":
		var sesCfg sesdeps.Config
		if err := envconfig.Process(ctx, &sesCfg); err != nil {
			return nil, err
		}
		return sesdeps.New(ctx, sesCfg)
	default:
		return nil, fmt.Errorf("email: unsupported fallback provider %q", cfg.Name)
	}
}
