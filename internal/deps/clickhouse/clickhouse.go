package clickhouse

import (
	"context"
	"fmt"
	"log/slog"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	chdriver "github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/slogx"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
	"go.opentelemetry.io/otel/trace"
)

func tracer() trace.Tracer {
	return otel.Tracer("clickhouse", trace.WithInstrumentationVersion("v1.0.0"))
}

type DB struct {
	Conn *Conn
}

type Conn struct {
	conn chdriver.Conn
}

var _ chdriver.Conn = (*Conn)(nil)

func (c *Conn) Unwrap() chdriver.Conn {
	return c.conn
}

func (c *Conn) withSpan(ctx context.Context) context.Context {
	if span := trace.SpanFromContext(ctx); span.SpanContext().IsValid() {
		return ch.Context(ctx, ch.WithSpan(span.SpanContext()))
	}
	return ctx
}

func (c *Conn) spanName(op string) string {
	return "ch." + op
}

func (c *Conn) setSpanAttrs(span trace.Span, query string) {
	span.SetAttributes(
		semconv.DBSystemKey.String("clickhouse"),
		attribute.String("db.query.text", query),
	)
}

func (c *Conn) Query(ctx context.Context, query string, args ...any) (chdriver.Rows, error) {
	ctx, span := tracer().Start(ctx, c.spanName("query"))
	defer func() { span.End() }()
	c.setSpanAttrs(span, query)
	rows, err := c.conn.Query(c.withSpan(ctx), query, args...)
	if err != nil {
		telemetry.RecordError(ctx, err)
	}
	return rows, err
}

// QueryRow is not traced because driver.Row defers errors to Scan(). A span
// here would always show success status (the span ends before Scan is called),
// which actively misleads operators. Callers should record Scan errors on their
// own spans via telemetry.RecordError if error visibility is needed.
func (c *Conn) QueryRow(ctx context.Context, query string, args ...any) chdriver.Row {
	return c.conn.QueryRow(c.withSpan(ctx), query, args...)
}

func (c *Conn) Exec(ctx context.Context, query string, args ...any) error {
	ctx, span := tracer().Start(ctx, c.spanName("exec"))
	defer func() { span.End() }()
	c.setSpanAttrs(span, query)
	err := c.conn.Exec(c.withSpan(ctx), query, args...)
	if err != nil {
		telemetry.RecordError(ctx, err)
	}
	return err
}

func (c *Conn) Select(ctx context.Context, dest any, query string, args ...any) error {
	ctx, span := tracer().Start(ctx, c.spanName("select"))
	defer func() { span.End() }()
	c.setSpanAttrs(span, query)
	err := c.conn.Select(c.withSpan(ctx), dest, query, args...)
	if err != nil {
		telemetry.RecordError(ctx, err)
	}
	return err
}

func (c *Conn) PrepareBatch(ctx context.Context, query string, opts ...chdriver.PrepareBatchOption) (chdriver.Batch, error) {
	ctx, span := tracer().Start(ctx, c.spanName("prepare_batch"))
	defer func() { span.End() }()
	c.setSpanAttrs(span, query)
	batch, err := c.conn.PrepareBatch(c.withSpan(ctx), query, opts...)
	if err != nil {
		telemetry.RecordError(ctx, err)
	}
	return batch, err
}

func (c *Conn) AsyncInsert(ctx context.Context, query string, wait bool, args ...any) error {
	ctx, span := tracer().Start(ctx, c.spanName("async_insert"))
	defer func() { span.End() }()
	c.setSpanAttrs(span, query)
	ctx = ch.Context(ctx, ch.WithAsync(wait))
	err := c.conn.Exec(c.withSpan(ctx), query, args...)
	if err != nil {
		telemetry.RecordError(ctx, err)
	}
	return err
}

func (c *Conn) Ping(ctx context.Context) error {
	return c.conn.Ping(ctx)
}

func (c *Conn) Stats() chdriver.Stats {
	return c.conn.Stats()
}

func (c *Conn) Close() error {
	return c.conn.Close()
}

func (c *Conn) ServerVersion() (*chdriver.ServerVersion, error) {
	return c.conn.ServerVersion()
}

func (c *Conn) Contributors() []string {
	return c.conn.Contributors()
}

func createConnection(ctx context.Context, cfg *Config) (*Conn, error) {
	opts, err := ch.ParseDSN(cfg.URL)
	if err != nil {
		slog.ErrorContext(ctx, "Unable to parse ClickHouse DSN", slogx.Error(err))
		return nil, err
	}

	conn, err := ch.Open(opts)
	if err != nil {
		slog.ErrorContext(ctx, "Unable to create ClickHouse connection", slogx.Error(err))
		return nil, err
	}

	if err := conn.Ping(ctx); err != nil {
		slog.ErrorContext(ctx, "Unable to ping ClickHouse", slogx.Error(err))
		if closeErr := conn.Close(); closeErr != nil {
			slog.ErrorContext(ctx, "failed to close ClickHouse after ping failure", slogx.Error(closeErr))
		}
		return nil, err
	}

	return &Conn{conn: conn}, nil
}

func NewReaderPool(ctx context.Context, cfg *Config) (*Conn, error) {
	return createConnection(ctx, cfg)
}

func NewWriterPool(ctx context.Context, cfg *Config) (*Conn, error) {
	return createConnection(ctx, cfg)
}

func NewFromConfig(ctx context.Context, cfg *Config) (*DB, error) {
	conn, err := createConnection(ctx, cfg)
	if err != nil {
		return nil, err
	}

	return &DB{Conn: conn}, nil
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
