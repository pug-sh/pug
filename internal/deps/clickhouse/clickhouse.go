package clickhouse

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/fivebitsio/cotton/internal/slogx"
)

type DB struct {
	Conn driver.Conn
}

func createConnection(ctx context.Context, cfg *Config) (driver.Conn, error) {
	opts, err := clickhouse.ParseDSN(cfg.URL)
	if err != nil {
		slog.ErrorContext(ctx, "Unable to parse ClickHouse DSN", slogx.Error(err))
		return nil, err
	}

	conn, err := clickhouse.Open(opts)
	if err != nil {
		slog.ErrorContext(ctx, "Unable to create ClickHouse connection", slogx.Error(err))
		return nil, err
	}

	if err := conn.Ping(ctx); err != nil {
		slog.ErrorContext(ctx, "Unable to ping ClickHouse", slogx.Error(err))
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
	slog.InfoContext(ctx, "Closing ClickHouse connection.")

	if db.Conn != nil {
		err := db.Conn.Close()
		if err != nil {
			slog.ErrorContext(ctx, "Error closing ClickHouse connection", slogx.Error(err))
			return fmt.Errorf("error closing ClickHouse connection: %w", err)
		}
	}
	return nil
}
