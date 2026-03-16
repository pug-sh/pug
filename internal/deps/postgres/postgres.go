package postgres

import (
	"context"
	"log/slog"

	"github.com/exaring/otelpgx"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DB holds pgx pool
type DB struct {
	Pool *pgxpool.Pool
}

func createPool(ctx context.Context, addr string) (*pgxpool.Pool, error) {
	dbPoolConfig, err := pgxpool.ParseConfig(addr)
	if err != nil {
		slog.ErrorContext(ctx, "unable to parse database URL", slogx.Error(err))
		return nil, err
	}

	dbPoolConfig.ConnConfig.Tracer = otelpgx.NewTracer()

	pool, err := pgxpool.NewWithConfig(context.Background(), dbPoolConfig)
	if err != nil {
		slog.ErrorContext(ctx, "unable to create connection pool", slogx.Error(err))
		return nil, err
	}
	return pool, nil
}

// NewFromConfig initiates DB connection from config and returns it
func NewFromConfig(ctx context.Context, cfg *Config) (*DB, error) {
	pool, err := createPool(ctx, cfg.URL)
	if err != nil {
		return nil, err
	}

	return &DB{Pool: pool}, nil
}

// NewReaderPool creates and returns a new PostgreSQL connection pool for read operations
func NewReaderPool(ctx context.Context, cfg *Config) (*pgxpool.Pool, error) {
	return createPool(ctx, cfg.URL)
}

// NewWriterPool creates and returns a new PostgreSQL connection pool for write operations
func NewWriterPool(ctx context.Context, cfg *Config) (*pgxpool.Pool, error) {
	return createPool(ctx, cfg.URL)
}

// Close closes opened DB connection pool
func (db *DB) Close(ctx context.Context) {
	slog.InfoContext(ctx, "Closing connection pool.")
	db.Pool.Close()
}
