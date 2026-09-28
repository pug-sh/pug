package auth

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	coreoauth "github.com/pug-sh/pug/internal/core/auth/oauth"
	"github.com/pug-sh/pug/internal/core/instance"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
)

// NewServiceForTest wires an auth Service with empty OAuth provider config and
// the demo login disabled (tests that exercise DemoSignIn flip it on via
// SetDemoEnabledForTest).
func NewServiceForTest(ctx context.Context, pgRO, pgW *pgxpool.Pool, jwtKey []byte, publisher JobPublisher, policies ...instance.Policy) (*Service, error) {
	policy := instance.OpenPolicy()
	if len(policies) != 0 {
		policy = policies[0]
	}
	return NewService(ctx, pgRO, pgW, jwtKey, publisher, coreoauth.Config{}, false, policy)
}

// NewServiceWithOAuthForTest wires an auth Service with a custom OAuth registry (integration tests).
func NewServiceWithOAuthForTest(
	ctx context.Context,
	pgRO, pgW *pgxpool.Pool,
	jwtKey []byte,
	publisher JobPublisher,
	oauthCfg coreoauth.Config,
	registry *coreoauth.Registry,
	policies ...instance.Policy,
) *Service {
	oauthSvc := coreoauth.NewService(oauthCfg, registry)
	policy := instance.OpenPolicy()
	if len(policies) != 0 {
		policy = policies[0]
	}
	return &Service{
		read:           dbread.New(pgRO),
		write:          dbwrite.New(pgW),
		pgW:            pgW,
		jwtKey:         jwtKey,
		publisher:      publisher,
		oauth:          oauthSvc,
		instancePolicy: policy,
	}
}
