package seed

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/sethvargo/go-envconfig"
)

type deps struct {
	pg *pgxpool.Pool
}

func (d *deps) close() {
	d.pg.Close()
}

func newDeps(ctx context.Context) (*deps, error) {
	var pgCfg postgres.Config
	if err := envconfig.Process(ctx, &pgCfg); err != nil {
		return nil, err
	}

	pg, err := postgres.NewWriterPool(ctx, &pgCfg)
	if err != nil {
		return nil, err
	}

	return &deps{pg: pg}, nil
}
