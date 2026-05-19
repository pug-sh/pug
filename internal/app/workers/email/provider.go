package email

import (
	"context"
	"fmt"
	"log/slog"

	coreemail "github.com/pug-sh/pug/internal/core/email"
	"github.com/pug-sh/pug/internal/core/email/fallback"
	"github.com/pug-sh/pug/internal/core/email/secret"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	goredis "github.com/redis/go-redis/v9"
	"github.com/sethvargo/go-envconfig"
)

type secretKeyConfig struct {
	KeyB64 string `env:"PUG_EMAIL_PROVIDER_SECRET_KEY"`
}

// fallbackProviderFactory is the constructor for the operator-default provider.
// Tests swap this; production wiring uses fallback.NewProvider.
var fallbackProviderFactory = fallback.NewProvider

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
