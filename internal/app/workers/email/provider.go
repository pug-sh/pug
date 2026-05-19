package email

import (
	"context"
	"fmt"
	"log/slog"

	coreemail "github.com/pug-sh/pug/internal/core/email"
	"github.com/pug-sh/pug/internal/core/email/secret"
	resenddeps "github.com/pug-sh/pug/internal/deps/email/resend"
	sesdeps "github.com/pug-sh/pug/internal/deps/email/ses"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	goredis "github.com/redis/go-redis/v9"
	"github.com/sethvargo/go-envconfig"
)

type providerConfig struct {
	Name string `env:"PUG_EMAIL_PROVIDER,default=resend"`
}

type secretKeyConfig struct {
	KeyB64 string `env:"PUG_EMAIL_PROVIDER_SECRET_KEY"`
}

// fallbackProviderFactory is the constructor for the operator-default provider.
// Tests swap this; production wiring uses newFallbackProvider.
var fallbackProviderFactory = newFallbackProvider

func newMailerWithResolver(ctx context.Context, read *dbread.Queries, cache *goredis.Client) (*coreemail.Service, error) {
	var emailCfg coreemail.Config
	if err := envconfig.Process(ctx, &emailCfg); err != nil {
		return nil, err
	}

	fallback, err := fallbackProviderFactory(ctx)
	if err != nil {
		return nil, err
	}

	var keyCfg secretKeyConfig
	if err := envconfig.Process(ctx, &keyCfg); err != nil {
		return nil, err
	}

	if keyCfg.KeyB64 == "" {
		slog.WarnContext(ctx, "PUG_EMAIL_PROVIDER_SECRET_KEY unset; per-tenant email providers disabled, using operator default for all sends")
		return coreemail.NewService(emailCfg, fallback)
	}

	cipher, err := secret.NewCipher(keyCfg.KeyB64)
	if err != nil {
		return nil, fmt.Errorf("init email cipher: %w", err)
	}
	repo := coreemail.NewOrgProviderRepo(read, cache)
	resolver := coreemail.NewTenantAwareResolver(repo, cipher, fallback, emailCfg.From, emailCfg.ReplyTo)

	return coreemail.NewServiceWithResolver(emailCfg, resolver)
}

func newFallbackProvider(ctx context.Context) (coreemail.Provider, error) {
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
		return nil, fmt.Errorf("email: unsupported fallback provider %q", cfg.Name)
	}
}
