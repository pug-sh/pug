package postgres

import (
	"context"

	"github.com/fivebitsio/cotton/pkg/logger"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DB holds pgx pool
type DB struct {
	Pool *pgxpool.Pool
}

// NewFromConfig initiates DB connection from config and returns it
func NewFromConfig(ctx context.Context, cfg *Config) (*DB, error) {
	pgxConfig, err := pgxpool.ParseConfig(cfg.ConnectionString())

	if err != nil {
		return nil, err
	}

	pool, err := pgxpool.NewWithConfig(ctx, pgxConfig)
	if err != nil {
		return nil, err
	}

	return &DB{Pool: pool}, nil
}

// Close closes opened DB connection pool
func (db *DB) Close(ctx context.Context) {
	logger := logger.FromContext(ctx)
	logger.Info("Closing connection pool.")
	db.Pool.Close()
}
