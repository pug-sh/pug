package auth

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	coreoauth "github.com/pug-sh/pug/internal/core/auth/oauth"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
)

// NewServiceForTest wires an auth Service with empty OAuth provider config.
func NewServiceForTest(ctx context.Context, pgRO, pgW *pgxpool.Pool, jwtKey []byte, publisher JobPublisher) (*Service, error) {
	return NewService(ctx, pgRO, pgW, jwtKey, publisher, coreoauth.Config{})
}

// NewServiceWithOAuthForTest wires an auth Service with a custom OAuth registry (integration tests).
func NewServiceWithOAuthForTest(
	ctx context.Context,
	pgRO, pgW *pgxpool.Pool,
	jwtKey []byte,
	publisher JobPublisher,
	oauthCfg coreoauth.Config,
	registry *coreoauth.Registry,
) *Service {
	oauthSvc := coreoauth.NewService(oauthCfg, registry)
	return &Service{
		read:      dbread.New(pgRO),
		write:     dbwrite.New(pgW),
		pgW:       pgW,
		jwtKey:    jwtKey,
		publisher: publisher,
		oauth:     oauthSvc,
	}
}
