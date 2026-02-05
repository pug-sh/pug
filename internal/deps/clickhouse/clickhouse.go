package clickhouse

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/fivebitsio/cotton/internal/deps/logger"
)

type DB struct {
	Conn driver.Conn
}

func createConnection(ctx context.Context, cfg *Config) (driver.Conn, error) {
	opts, err := clickhouse.ParseDSN(cfg.DSN())
	if err != nil {
		logger := logger.FromContext(ctx)
		logger.Error("Unable to parse ClickHouse DSN", slog.Any("error", err))
		return nil, err
	}

	conn, err := clickhouse.Open(opts)
	if err != nil {
		logger := logger.FromContext(ctx)
		logger.Error("Unable to create ClickHouse connection", slog.Any("error", err))
		return nil, err
	}

	if err := conn.Ping(ctx); err != nil {
		logger := logger.FromContext(ctx)
		logger.Error("Unable to ping ClickHouse", slog.Any("error", err))
		return nil, err
	}

	return conn, nil
}

func NewFromConfig(ctx context.Context, cfg *Config) (*DB, error) {
	conn, err := createConnection(ctx, cfg)
	if err != nil {
		return nil, err
	}

	return &DB{Conn: conn}, nil
}

func NewReaderPool(ctx context.Context, cfg *Config) (driver.Conn, error) {
	return createConnection(ctx, cfg)
}

func NewWriterPool(ctx context.Context, cfg *Config) (driver.Conn, error) {
	return createConnection(ctx, cfg)
}

func (db *DB) Close(ctx context.Context) error {
	logger := logger.FromContext(ctx)
	logger.Info("Closing ClickHouse connection.")

	if db.Conn != nil {
		err := db.Conn.Close()
		if err != nil {
			logger.Error("Error closing ClickHouse connection", slog.Any("error", err))
			return fmt.Errorf("error closing ClickHouse connection: %w", err)
		}
	}
	return nil
}
