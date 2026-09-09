package server

import (
	"context"
	"fmt"

	goredis "github.com/redis/go-redis/v9"

	coreemail "github.com/pug-sh/pug/internal/core/email"
	"github.com/pug-sh/pug/internal/core/email/fallback"
	"github.com/pug-sh/pug/internal/core/email/secret"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/sethvargo/go-envconfig"
)

type emailKeyConfig struct {
	KeyB64 string `env:"PUG_EMAIL_PROVIDER_SECRET_KEY"`
}

// emailDeps is zero-valued when no secret key is configured; the handler's own
// requireCipher gate then answers FailedPrecondition instead of failing startup.
type emailDeps struct {
	cipher *secret.Cipher
	repo   *coreemail.OrgProviderRepo
	mailer *coreemail.Service
}

func newEmail(ctx context.Context, read *dbread.Queries, cache *goredis.Client) (emailDeps, error) {
	var keyCfg emailKeyConfig
	if err := envconfig.Process(ctx, &keyCfg); err != nil {
		return emailDeps{}, err
	}
	if keyCfg.KeyB64 == "" {
		return emailDeps{}, nil
	}

	cipher, err := secret.NewCipher(keyCfg.KeyB64)
	if err != nil {
		return emailDeps{}, fmt.Errorf("init email cipher: %w", err)
	}

	var cfg coreemail.Config
	if err := envconfig.Process(ctx, &cfg); err != nil {
		return emailDeps{}, err
	}
	fallbackProvider, err := fallback.NewProvider(ctx)
	if err != nil {
		return emailDeps{}, err
	}
	repo := coreemail.NewOrgProviderRepo(read, cache)
	resolver, err := coreemail.NewTenantAwareResolver(repo, cipher, fallbackProvider, cfg.From, cfg.ReplyTo)
	if err != nil {
		return emailDeps{}, err
	}
	mailer, err := coreemail.NewServiceWithResolver(cfg, resolver)
	if err != nil {
		return emailDeps{}, err
	}
	return emailDeps{cipher: cipher, repo: repo, mailer: mailer}, nil
}
