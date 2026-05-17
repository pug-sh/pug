package email

import (
	"context"
	"fmt"

	coreemail "github.com/pug-sh/pug/internal/core/email"
	resenddeps "github.com/pug-sh/pug/internal/deps/email/resend"
	sesdeps "github.com/pug-sh/pug/internal/deps/email/ses"
	"github.com/sethvargo/go-envconfig"
)

type providerConfig struct {
	Name string `env:"PUG_EMAIL_PROVIDER,default=resend"`
}

var providerFactory = newProvider

func newMailer(ctx context.Context) (*coreemail.Service, error) {
	var emailCfg coreemail.Config
	if err := envconfig.Process(ctx, &emailCfg); err != nil {
		return nil, err
	}

	provider, err := providerFactory(ctx)
	if err != nil {
		return nil, err
	}

	return coreemail.NewService(emailCfg, provider)
}

func newProvider(ctx context.Context) (coreemail.Provider, error) {
	var cfg providerConfig
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
		return nil, fmt.Errorf("email: unsupported provider %q", cfg.Name)
	}
}
