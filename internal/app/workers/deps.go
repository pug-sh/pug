package workers

import (
	"context"

	"github.com/fivebitsio/cotton/internal/deps/nats"
	"github.com/fivebitsio/cotton/internal/deps/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sethvargo/go-envconfig"
)

type deps struct {
	pgRo *pgxpool.Pool
	pgW  *pgxpool.Pool
	nats *nats.NATSClient
}

func newDeps(ctx context.Context) (*deps, error) {
	var cfg postgres.Config
	if err := envconfig.Process(ctx, &cfg); err != nil {
		return nil, err
	}

	pgRo, err := postgres.NewReaderPool(ctx, &cfg)
	if err != nil {
		return nil, err
	}

	pgW, err := postgres.NewWriterPool(ctx, &cfg)
	if err != nil {
		return nil, err
	}

	natsClient, err := nats.New(ctx)
	if err != nil {
		return nil, err
	}

	return &deps{
		pgRo: pgRo,
		pgW:  pgW,
		nats: natsClient,
	}, nil
}

func (d *deps) close(ctx context.Context) {
	d.pgRo.Close()
	d.pgW.Close()
	if d.nats != nil {
		d.nats.Close()
	}
}
