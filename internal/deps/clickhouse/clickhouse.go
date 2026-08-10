package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	// Embedded, not implemented: driver.Conn gains methods in minor releases,
	// and untraced forwarding beats a build that stops on a version bump.
	chdriver.Conn
}

var _ chdriver.Conn = (*Conn)(nil)

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

// The driver returns once the first block lands and reports later failures via
// Rows.Err(), so the span outlives this call and ends on Rows.Close.
func (c *Conn) Query(ctx context.Context, query string, args ...any) (chdriver.Rows, error) {
	ctx, span := tracer().Start(ctx, c.spanName("query"))
	c.setSpanAttrs(span, query)
	rows, err := c.Conn.Query(c.withSpan(ctx), query, args...)
	if err != nil {
		telemetry.RecordError(ctx, err)
		span.End()
		return nil, err
	}
	return &tracedRows{Rows: rows, span: span}, nil
}

// QueryRow is not traced because driver.Row defers errors to Scan(). A span
// here would always show success status (the span ends before Scan is called),
// which actively misleads operators. Callers should record Scan errors on their
// own spans via telemetry.RecordError if error visibility is needed.
func (c *Conn) QueryRow(ctx context.Context, query string, args ...any) chdriver.Row {
	return c.Conn.QueryRow(c.withSpan(ctx), query, args...)
}

func (c *Conn) Exec(ctx context.Context, query string, args ...any) error {
	ctx, span := tracer().Start(ctx, c.spanName("exec"))
	defer func() { span.End() }()
	c.setSpanAttrs(span, query)
	err := c.Conn.Exec(c.withSpan(ctx), query, args...)
	if err != nil {
		telemetry.RecordError(ctx, err)
	}
	return err
}

func (c *Conn) Select(ctx context.Context, dest any, query string, args ...any) error {
	ctx, span := tracer().Start(ctx, c.spanName("select"))
	defer func() { span.End() }()
	c.setSpanAttrs(span, query)
	err := c.Conn.Select(c.withSpan(ctx), dest, query, args...)
	if err != nil {
		telemetry.RecordError(ctx, err)
	}
	return err
}

// PrepareBatch writes no rows, so the insert's outcome only shows up later.
// The span ends on the first of Send/Abort/Close.
func (c *Conn) PrepareBatch(ctx context.Context, query string, opts ...chdriver.PrepareBatchOption) (chdriver.Batch, error) {
	ctx, span := tracer().Start(ctx, c.spanName("prepare_batch"))
	c.setSpanAttrs(span, query)
	batch, err := c.Conn.PrepareBatch(c.withSpan(ctx), query, opts...)
	if err != nil {
		telemetry.RecordError(ctx, err)
		span.End()
		return nil, err
	}
	return &tracedBatch{Batch: batch, span: span}, nil
}

// QueryFormat and InsertFormat are HTTP-only — a native DSN gets
// ErrFormatNativeUnsupported before the pool is touched. QueryFormat's span
// ends on the stream's Close, since a mid-stream failure surfaces from Read.
func (c *Conn) QueryFormat(ctx context.Context, format string, query string, args ...any) (io.ReadCloser, error) {
	ctx, span := tracer().Start(ctx, c.spanName("query_format"))
	c.setSpanAttrs(span, query)
	span.SetAttributes(attribute.String("db.clickhouse.format", format))
	stream, err := c.Conn.QueryFormat(c.withSpan(ctx), format, query, args...)
	if err != nil {
		telemetry.RecordError(ctx, err)
		span.End()
		return nil, err
	}
	return &formatStream{ReadCloser: stream, span: span}, nil
}

func (c *Conn) InsertFormat(ctx context.Context, format string, query string, data io.Reader) error {
	ctx, span := tracer().Start(ctx, c.spanName("insert_format"))
	defer func() { span.End() }()
	c.setSpanAttrs(span, query)
	span.SetAttributes(attribute.String("db.clickhouse.format", format))
	err := c.Conn.InsertFormat(c.withSpan(ctx), format, query, data)
	if err != nil {
		telemetry.RecordError(ctx, err)
	}
	return err
}

func (c *Conn) AsyncInsert(ctx context.Context, query string, wait bool, args ...any) error {
	ctx, span := tracer().Start(ctx, c.spanName("async_insert"))
	defer func() { span.End() }()
	c.setSpanAttrs(span, query)
	ctx = ch.Context(ctx, ch.WithAsync(wait))
	err := c.Conn.Exec(c.withSpan(ctx), query, args...)
	if err != nil {
		telemetry.RecordError(ctx, err)
	}
	return err
}

type formatStream struct {
	io.ReadCloser
	span     trace.Span
	recorded bool
}

func (s *formatStream) record(err error) {
	if err == nil || s.recorded {
		return
	}
	s.recorded = true
	telemetry.RecordErrorOnSpan(s.span, err)
}

func (s *formatStream) Read(p []byte) (int, error) {
	n, err := s.ReadCloser.Read(p)
	if !errors.Is(err, io.EOF) {
		s.record(err)
	}
	return n, err
}

func (s *formatStream) Close() error {
	defer s.span.End()
	err := s.ReadCloser.Close()
	s.record(err)
	return err
}

// Scan is deliberately not wrapped: its errors are the caller's (wrong dest
// type, Scan before Next), not the query's. Close carries the late error too,
// so a caller that skips Err() still gets it recorded.
type tracedRows struct {
	chdriver.Rows
	span     trace.Span
	recorded bool
}

func (r *tracedRows) record(err error) {
	if err == nil || r.recorded {
		return
	}
	r.recorded = true
	telemetry.RecordErrorOnSpan(r.span, err)
}

func (r *tracedRows) Err() error {
	err := r.Rows.Err()
	r.record(err)
	return err
}

func (r *tracedRows) Close() error {
	defer r.span.End()
	err := r.Rows.Close()
	r.record(err)
	return err
}

type tracedBatch struct {
	chdriver.Batch
	span     trace.Span
	recorded bool
	ended    bool
}

// Finalizers routinely run twice — a failed Send then a deferred Abort, which
// returns the meaningless ErrBatchAlreadySent — so first error wins.
func (b *tracedBatch) record(err error) {
	if err == nil || b.recorded || b.ended {
		return
	}
	b.recorded = true
	telemetry.RecordErrorOnSpan(b.span, err)
}

func (b *tracedBatch) finish(err error) {
	if b.ended {
		return
	}
	b.record(err)
	b.ended = true
	b.span.End()
}

// Append looks per-row but is batch-fatal, and the Abort that follows reports
// nil — without recording here a failed insert would close green.
func (b *tracedBatch) Append(v ...any) error {
	err := b.Batch.Append(v...)
	b.record(err)
	return err
}

func (b *tracedBatch) AppendStruct(v any) error {
	err := b.Batch.AppendStruct(v)
	b.record(err)
	return err
}

// Flush sends a block but leaves the batch usable, so it records without ending.
func (b *tracedBatch) Flush() error {
	err := b.Batch.Flush()
	b.record(err)
	return err
}

func (b *tracedBatch) Send() error {
	err := b.Batch.Send()
	b.finish(err)
	return err
}

func (b *tracedBatch) Abort() error {
	err := b.Batch.Abort()
	b.finish(err)
	return err
}

func (b *tracedBatch) Close() error {
	err := b.Batch.Close()
	b.finish(err)
	return err
}

// AbortUnsent finalizes a batch no path sent — an abandoned batch holds its
// pooled connection and never ends its span. A failed Send still finalizes the
// batch driver-side, so gate on IsSent or the abort logs ErrBatchAlreadySent.
func AbortUnsent(ctx context.Context, batch chdriver.Batch, attrs ...slog.Attr) {
	if batch == nil || batch.IsSent() {
		return
	}
	err := batch.Abort()
	if err == nil {
		return
	}
	slog.LogAttrs(ctx, slog.LevelError, "failed to abort ClickHouse batch",
		append([]slog.Attr{slogx.Error(err)}, attrs...)...)
	telemetry.RecordError(ctx, err)
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

	return &Conn{Conn: conn}, nil
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
