// Package fallback constructs the operator-default email provider from
// environment configuration. It is the bottom of every tenant-aware resolver
// chain.
package fallback

import (
	"context"
	"fmt"

	coreemail "github.com/pug-sh/pug/internal/core/email"
	resenddeps "github.com/pug-sh/pug/internal/deps/email/resend"
	sesdeps "github.com/pug-sh/pug/internal/deps/email/ses"
	"github.com/sethvargo/go-envconfig"
)

// ProviderName is the closed set of operator-default providers selectable
// via PUG_EMAIL_PROVIDER. Mirrors coreemail.ProviderKind.Valid() pattern.
type ProviderName string

const (
	ProviderResend ProviderName = "resend"
	ProviderSES    ProviderName = "ses"
)

func (n ProviderName) Valid() bool {
	switch n {
	case ProviderResend, ProviderSES:
		return true
	}
	return false
}

type Config struct {
	Name ProviderName `env:"PUG_EMAIL_PROVIDER,default=resend"`
}

// NewProvider builds the operator-configured Provider chosen by
// PUG_EMAIL_PROVIDER (resend | ses). Used as the fallback when no per-tenant
// provider is set for an org.
func NewProvider(ctx context.Context) (coreemail.Provider, error) {
	var cfg Config
	if err := envconfig.Process(ctx, &cfg); err != nil {
		return nil, err
	}
	if !cfg.Name.Valid() {
		return nil, fmt.Errorf("email: unsupported fallback provider %q (set PUG_EMAIL_PROVIDER to one of: resend, ses)", cfg.Name)
	}
	switch cfg.Name {
	case ProviderResend:
		var resendCfg resenddeps.Config
		if err := envconfig.Process(ctx, &resendCfg); err != nil {
			return nil, err
		}
		return resenddeps.New(resendCfg)
	case ProviderSES:
		var sesCfg sesdeps.Config
		if err := envconfig.Process(ctx, &sesCfg); err != nil {
			return nil, err
		}
		return sesdeps.New(ctx, sesCfg)
	}
	// Unreachable because of Valid() above, but the compiler doesn't know that.
	return nil, fmt.Errorf("email: unsupported fallback provider %q", cfg.Name)
}
