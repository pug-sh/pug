package main

import (
	"context"

	"github.com/fivebitsio/cotton/pkg/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sethvargo/go-envconfig"
)

type dependencies struct {
	pgRo   *pgxpool.Pool
	pgW    *pgxpool.Pool
	jwtKey []byte
}

func newDependencies(ctx context.Context) (*dependencies, error) {
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

	jwtKey := []byte("your-jwt-secret-key-here")

	return &dependencies{
		pgRo:   pgRo,
		pgW:    pgW,
		jwtKey: jwtKey,
	}, nil
}

func (deps *dependencies) Close(ctx context.Context) {
	deps.pgRo.Close()
	deps.pgW.Close()
}
