// Package projects is the operator CLI behind `pug projects`.
package projects

import (
	"context"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5/pgxpool"
	coreprojects "github.com/pug-sh/pug/internal/core/projects"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/redis"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/rs/xid"
	"github.com/sethvargo/go-envconfig"
)

type CLI struct {
	svc   *coreprojects.Service
	pgW   *pgxpool.Pool
	redis *redis.Client
}

// New builds its own pools: this binary runs one command and exits. Redis is
// there to drop a deleted project's cached keys, as the RPC does.
func New(ctx context.Context) (*CLI, error) {
	var pgCfg postgres.Config
	if err := envconfig.Process(ctx, &pgCfg); err != nil {
		return nil, fmt.Errorf("postgres config: %w", err)
	}
	var redisCfg redis.Config
	if err := envconfig.Process(ctx, &redisCfg); err != nil {
		return nil, fmt.Errorf("redis config: %w", err)
	}
	pgW, err := postgres.NewWriterPool(ctx, &pgCfg)
	if err != nil {
		return nil, fmt.Errorf("postgres writer pool: %w", err)
	}
	rd, err := redis.NewFromConfig(ctx, &redisCfg)
	if err != nil {
		pgW.Close()
		return nil, fmt.Errorf("redis: %w", err)
	}
	repo := coreprojects.NewRepo(dbread.New(pgW), rd.Unwrap())
	return &CLI{svc: coreprojects.NewService(pgW, pgW, repo), pgW: pgW, redis: rd}, nil
}

func (c *CLI) Close(ctx context.Context) {
	c.redis.Close(ctx)
	c.pgW.Close()
}

// Delete deletes the project as its admin would, or queues an id with no
// projects row.
func (c *CLI) Delete(ctx context.Context, out io.Writer, projectID, actor string) error {
	// A typo would otherwise queue a deletion that matches nothing.
	if _, err := xid.FromString(projectID); err != nil {
		return fmt.Errorf("%q is not a project id", projectID)
	}
	did, err := c.svc.DeleteProjectByOperator(ctx, projectID, actor)
	if err != nil {
		return err
	}
	var msg string
	switch did {
	case coreprojects.OperatorDeletionHidden:
		msg = "hidden, its keys, share links and push credential revoked, and its data queued for erasure"
	case coreprojects.OperatorDeletionQueuedOrphan:
		msg = "no projects row; queued, so the purge job erases what ClickHouse holds"
	case coreprojects.OperatorDeletionReopened:
		msg = "its finished deletion is reopened"
	case coreprojects.OperatorDeletionUnchanged:
		msg = "its deletion is already open; nothing changed"
	}
	_, err = fmt.Fprintf(out, "%s: %s\n", projectID, msg)
	return err
}
