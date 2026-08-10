package clickhouse

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	chdriver "github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func setupTracing(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(old)
		_ = tp.Shutdown(context.Background())
	})
	return exporter
}

// fakeConn records what it was handed. The wrapper passes (format, query) as
// two adjacent strings, so a swap has to be caught by assertion — it compiles
// and vets clean either way.
type fakeConn struct {
	chdriver.Conn

	stream io.ReadCloser
	rows   chdriver.Rows
	batch  chdriver.Batch
	err    error

	gotFormat string
	gotQuery  string
	gotArgs   []any
	gotData   io.Reader
}

func (f *fakeConn) QueryFormat(_ context.Context, format string, query string, args ...any) (io.ReadCloser, error) {
	f.gotFormat, f.gotQuery, f.gotArgs = format, query, args
	if f.err != nil {
		return nil, f.err
	}
	return f.stream, nil
}

func (f *fakeConn) InsertFormat(_ context.Context, format string, query string, data io.Reader) error {
	f.gotFormat, f.gotQuery, f.gotData = format, query, data
	return f.err
}

func (f *fakeConn) Query(_ context.Context, query string, args ...any) (chdriver.Rows, error) {
	f.gotQuery, f.gotArgs = query, args
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

func (f *fakeConn) PrepareBatch(_ context.Context, query string, _ ...chdriver.PrepareBatchOption) (chdriver.Batch, error) {
	f.gotQuery = query
	if f.err != nil {
		return nil, f.err
	}
	return f.batch, nil
}

type fakeStream struct {
	io.Reader
	closed   bool
	closeErr error
}

func (s *fakeStream) Close() error {
	s.closed = true
	return s.closeErr
}

type errAfterReader struct{ err error }

func (r errAfterReader) Read([]byte) (int, error) { return 0, r.err }

// fakeRows mirrors the driver: a failure after the first block lands in err and
// is served by both Err and Close, long after Query returned.
type fakeRows struct {
	chdriver.Rows
	err    error
	closed bool
}

func (r *fakeRows) Err() error { return r.err }

func (r *fakeRows) Close() error {
	r.closed = true
	return r.err
}

// Finalization mirrors the driver: a failed Send still marks the batch sent, and
// a second finalizer reports ErrBatchAlreadySent or nil, not the original error.
type fakeBatch struct {
	chdriver.Batch
	sendErr   error
	appendErr error
	flushErr  error
	closeErr  error
	sent      bool
	aborted   bool
	closed    bool
}

func (b *fakeBatch) Append(...any) error { return b.appendErr }

func (b *fakeBatch) IsSent() bool { return b.sent }

func (b *fakeBatch) Flush() error { return b.flushErr }

func (b *fakeBatch) Send() error {
	b.sent = true
	return b.sendErr
}

func (b *fakeBatch) Abort() error {
	b.aborted = true
	if b.sent {
		return ch.ErrBatchAlreadySent
	}
	b.sent = true
	return nil
}

func (b *fakeBatch) Close() error {
	b.closed = true
	if b.sent {
		return nil
	}
	b.sent = true
	return b.closeErr
}

func exceptionCount(events []sdktrace.Event) int {
	n := 0
	for _, e := range events {
		if e.Name == "exception" {
			n++
		}
	}
	return n
}

func onlySpan(t *testing.T, exporter *tracetest.InMemoryExporter) tracetest.SpanStub {
	t.Helper()
	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	return spans[0]
}

func spanAttr(t *testing.T, span tracetest.SpanStub, key string) string {
	t.Helper()
	for _, a := range span.Attributes {
		if string(a.Key) == key {
			return a.Value.AsString()
		}
	}
	t.Fatalf("span %q has no %s attribute", span.Name, key)
	return ""
}

func TestQueryFormatSpanEndsOnCloseNotOnReturn(t *testing.T) {
	exporter := setupTracing(t)
	c := &Conn{Conn: &fakeConn{stream: &fakeStream{Reader: strings.NewReader("a,b\n")}}}

	stream, err := c.QueryFormat(context.Background(), "CSV", "select 1")
	if err != nil {
		t.Fatalf("QueryFormat: %v", err)
	}
	if got := len(exporter.GetSpans()); got != 0 {
		t.Fatalf("span ended before the stream was consumed: got %d spans, want 0", got)
	}

	if _, err := io.ReadAll(stream); err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if got := len(exporter.GetSpans()); got != 0 {
		t.Fatalf("span ended at EOF rather than Close: got %d spans, want 0", got)
	}

	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	span := onlySpan(t, exporter)
	if span.Name != "ch.query_format" {
		t.Errorf("span name = %q, want ch.query_format", span.Name)
	}
	if span.Status.Code == codes.Error {
		t.Errorf("clean read produced an error span: %q", span.Status.Description)
	}
	if n := exceptionCount(span.Events); n != 0 {
		t.Errorf("clean read recorded %d exceptions, want 0", n)
	}
}

func TestQueryFormatPassesFormatAndQueryInOrder(t *testing.T) {
	exporter := setupTracing(t)
	fake := &fakeConn{stream: &fakeStream{Reader: strings.NewReader("")}}
	c := &Conn{Conn: fake}

	stream, err := c.QueryFormat(context.Background(), "CSV", "select 1", 42)
	if err != nil {
		t.Fatalf("QueryFormat: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if fake.gotFormat != "CSV" {
		t.Errorf("format = %q, want CSV", fake.gotFormat)
	}
	if fake.gotQuery != "select 1" {
		t.Errorf("query = %q, want %q", fake.gotQuery, "select 1")
	}
	if len(fake.gotArgs) != 1 || fake.gotArgs[0] != 42 {
		t.Errorf("args = %v, want [42]", fake.gotArgs)
	}

	span := onlySpan(t, exporter)
	if got := spanAttr(t, span, "db.clickhouse.format"); got != "CSV" {
		t.Errorf("db.clickhouse.format = %q, want CSV", got)
	}
	if got := spanAttr(t, span, "db.query.text"); got != "select 1" {
		t.Errorf("db.query.text = %q, want %q", got, "select 1")
	}
}

func TestQueryFormatRecordsMidStreamErrorOnce(t *testing.T) {
	exporter := setupTracing(t)
	streamErr := errors.New("code: 241, MEMORY_LIMIT_EXCEEDED")
	c := &Conn{Conn: &fakeConn{stream: &fakeStream{Reader: errAfterReader{err: streamErr}}}}

	stream, err := c.QueryFormat(context.Background(), "CSV", "select 1")
	if err != nil {
		t.Fatalf("QueryFormat: %v", err)
	}

	buf := make([]byte, 8)
	for range 3 {
		if _, err := stream.Read(buf); !errors.Is(err, streamErr) {
			t.Fatalf("Read err = %v, want %v", err, streamErr)
		}
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	span := onlySpan(t, exporter)
	if span.Status.Code != codes.Error {
		t.Errorf("mid-stream failure left span status %v, want Error", span.Status.Code)
	}
	if span.Status.Description != streamErr.Error() {
		t.Errorf("status description = %q, want %q", span.Status.Description, streamErr.Error())
	}
	if n := exceptionCount(span.Events); n != 1 {
		t.Errorf("recorded %d exceptions across 3 failing reads, want 1", n)
	}
}

func TestQueryFormatEagerErrorEndsSpanAndReturnsNilStream(t *testing.T) {
	exporter := setupTracing(t)
	openErr := errors.New("format not supported over native protocol")
	c := &Conn{Conn: &fakeConn{err: openErr}}

	stream, err := c.QueryFormat(context.Background(), "CSV", "select 1")
	if !errors.Is(err, openErr) {
		t.Fatalf("err = %v, want %v", err, openErr)
	}
	if stream != nil {
		t.Errorf("stream = %v, want nil on error", stream)
	}

	span := onlySpan(t, exporter)
	if span.Status.Code != codes.Error {
		t.Errorf("span status = %v, want Error", span.Status.Code)
	}
}

func TestQueryFormatCloseReleasesUnderlyingStream(t *testing.T) {
	setupTracing(t)
	underlying := &fakeStream{Reader: strings.NewReader("a")}
	c := &Conn{Conn: &fakeConn{stream: underlying}}

	stream, err := c.QueryFormat(context.Background(), "CSV", "select 1")
	if err != nil {
		t.Fatalf("QueryFormat: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !underlying.closed {
		t.Error("Close did not reach the underlying stream, leaking a pooled connection")
	}
}

func TestInsertFormatPassesFormatAndQueryInOrder(t *testing.T) {
	exporter := setupTracing(t)
	fake := &fakeConn{}
	c := &Conn{Conn: fake}
	data := strings.NewReader("a,b\n")

	if err := c.InsertFormat(context.Background(), "CSV", "insert into t", data); err != nil {
		t.Fatalf("InsertFormat: %v", err)
	}

	if fake.gotFormat != "CSV" {
		t.Errorf("format = %q, want CSV", fake.gotFormat)
	}
	if fake.gotQuery != "insert into t" {
		t.Errorf("query = %q, want %q", fake.gotQuery, "insert into t")
	}
	if fake.gotData != data {
		t.Error("InsertFormat did not forward the data reader")
	}

	span := onlySpan(t, exporter)
	if span.Name != "ch.insert_format" {
		t.Errorf("span name = %q, want ch.insert_format", span.Name)
	}
	if got := spanAttr(t, span, "db.clickhouse.format"); got != "CSV" {
		t.Errorf("db.clickhouse.format = %q, want CSV", got)
	}
	if span.Status.Code == codes.Error {
		t.Errorf("clean insert produced an error span: %q", span.Status.Description)
	}
}

func TestInsertFormatRecordsError(t *testing.T) {
	exporter := setupTracing(t)
	insertErr := errors.New("format not supported over native protocol")
	c := &Conn{Conn: &fakeConn{err: insertErr}}

	err := c.InsertFormat(context.Background(), "CSV", "insert into t", strings.NewReader(""))
	if !errors.Is(err, insertErr) {
		t.Fatalf("err = %v, want %v", err, insertErr)
	}

	span := onlySpan(t, exporter)
	if span.Status.Code != codes.Error {
		t.Errorf("span status = %v, want Error", span.Status.Code)
	}
	if n := exceptionCount(span.Events); n != 1 {
		t.Errorf("recorded %d exceptions, want 1", n)
	}
}

func TestQuerySpanEndsOnRowsCloseNotOnReturn(t *testing.T) {
	exporter := setupTracing(t)
	underlying := &fakeRows{}
	c := &Conn{Conn: &fakeConn{rows: underlying}}

	rows, err := c.Query(context.Background(), "select 1")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := len(exporter.GetSpans()); got != 0 {
		t.Fatalf("span ended before the rows were consumed: got %d spans, want 0", got)
	}

	if err := rows.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !underlying.closed {
		t.Error("Close did not reach the underlying rows")
	}

	span := onlySpan(t, exporter)
	if span.Name != "ch.query" {
		t.Errorf("span name = %q, want ch.query", span.Name)
	}
	if span.Status.Code == codes.Error {
		t.Errorf("clean query produced an error span: %q", span.Status.Description)
	}
}

func TestQueryRecordsLateErrorOnce(t *testing.T) {
	exporter := setupTracing(t)
	lateErr := errors.New("code: 241, MEMORY_LIMIT_EXCEEDED")
	c := &Conn{Conn: &fakeConn{rows: &fakeRows{err: lateErr}}}

	rows, err := c.Query(context.Background(), "select 1")
	if err != nil {
		t.Fatalf("Query returned an eager error, but the failure is mid-query: %v", err)
	}
	if err := rows.Err(); !errors.Is(err, lateErr) {
		t.Fatalf("Err() = %v, want %v", err, lateErr)
	}
	if err := rows.Close(); !errors.Is(err, lateErr) {
		t.Fatalf("Close() = %v, want %v", err, lateErr)
	}

	span := onlySpan(t, exporter)
	if span.Status.Code != codes.Error {
		t.Errorf("late failure left span status %v, want Error", span.Status.Code)
	}
	if span.Status.Description != lateErr.Error() {
		t.Errorf("status description = %q, want %q", span.Status.Description, lateErr.Error())
	}
	if n := exceptionCount(span.Events); n != 1 {
		t.Errorf("recorded %d exceptions across Err+Close, want 1", n)
	}
}

func TestQueryEagerErrorEndsSpanAndReturnsNilRows(t *testing.T) {
	exporter := setupTracing(t)
	openErr := errors.New("code: 62, SYNTAX_ERROR")
	c := &Conn{Conn: &fakeConn{err: openErr}}

	rows, err := c.Query(context.Background(), "select")
	if !errors.Is(err, openErr) {
		t.Fatalf("err = %v, want %v", err, openErr)
	}
	if rows != nil {
		t.Errorf("rows = %v, want nil on error", rows)
	}

	span := onlySpan(t, exporter)
	if span.Status.Code != codes.Error {
		t.Errorf("span status = %v, want Error", span.Status.Code)
	}
}

func TestPrepareBatchSpanEndsOnSendNotOnReturn(t *testing.T) {
	exporter := setupTracing(t)
	c := &Conn{Conn: &fakeConn{batch: &fakeBatch{}}}

	batch, err := c.PrepareBatch(context.Background(), "insert into events")
	if err != nil {
		t.Fatalf("PrepareBatch: %v", err)
	}
	if got := len(exporter.GetSpans()); got != 0 {
		t.Fatalf("span ended before Send: got %d spans, want 0", got)
	}

	if err := batch.Send(); err != nil {
		t.Fatalf("Send: %v", err)
	}
	span := onlySpan(t, exporter)
	if span.Name != "ch.prepare_batch" {
		t.Errorf("span name = %q, want ch.prepare_batch", span.Name)
	}
	if span.Status.Code == codes.Error {
		t.Errorf("clean send produced an error span: %q", span.Status.Description)
	}

	if err := batch.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := len(exporter.GetSpans()); got != 1 {
		t.Errorf("a deferred Close after Send exported %d spans, want 1", got)
	}
}

func TestPrepareBatchRecordsSendError(t *testing.T) {
	exporter := setupTracing(t)
	sendErr := errors.New("code: 252, TOO_MANY_PARTS")
	c := &Conn{Conn: &fakeConn{batch: &fakeBatch{sendErr: sendErr}}}

	batch, err := c.PrepareBatch(context.Background(), "insert into events")
	if err != nil {
		t.Fatalf("PrepareBatch: %v", err)
	}
	if err := batch.Send(); !errors.Is(err, sendErr) {
		t.Fatalf("Send() = %v, want %v", err, sendErr)
	}

	span := onlySpan(t, exporter)
	if span.Status.Code != codes.Error {
		t.Errorf("failed send left span status %v, want Error", span.Status.Code)
	}
	if span.Status.Description != sendErr.Error() {
		t.Errorf("status description = %q, want %q", span.Status.Description, sendErr.Error())
	}
}

func TestPrepareBatchSpanEndsOnAbort(t *testing.T) {
	exporter := setupTracing(t)
	underlying := &fakeBatch{}
	c := &Conn{Conn: &fakeConn{batch: underlying}}

	batch, err := c.PrepareBatch(context.Background(), "insert into events")
	if err != nil {
		t.Fatalf("PrepareBatch: %v", err)
	}
	if err := batch.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if !underlying.aborted {
		t.Error("Abort did not reach the underlying batch")
	}
	if got := len(exporter.GetSpans()); got != 1 {
		t.Fatalf("got %d spans after Abort, want 1", got)
	}
}

func TestPrepareBatchAppendErrorSurvivesCleanAbort(t *testing.T) {
	exporter := setupTracing(t)
	appendErr := errors.New("code: 53, TYPE_MISMATCH")
	c := &Conn{Conn: &fakeConn{batch: &fakeBatch{appendErr: appendErr}}}

	batch, err := c.PrepareBatch(context.Background(), "insert into events")
	if err != nil {
		t.Fatalf("PrepareBatch: %v", err)
	}
	if err := batch.Append("bad"); !errors.Is(err, appendErr) {
		t.Fatalf("Append() = %v, want %v", err, appendErr)
	}
	if got := len(exporter.GetSpans()); got != 0 {
		t.Fatalf("Append ended the span; the batch still needs a finalizer: got %d spans, want 0", got)
	}
	if err := batch.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}

	span := onlySpan(t, exporter)
	if span.Status.Code != codes.Error {
		t.Errorf("a nil-returning Abort closed a failed insert with status %v, want Error", span.Status.Code)
	}
	if span.Status.Description != appendErr.Error() {
		t.Errorf("status description = %q, want %q", span.Status.Description, appendErr.Error())
	}
}

func TestPrepareBatchSendErrorSurvivesLaterAbort(t *testing.T) {
	exporter := setupTracing(t)
	sendErr := errors.New("code: 252, TOO_MANY_PARTS")
	c := &Conn{Conn: &fakeConn{batch: &fakeBatch{sendErr: sendErr}}}

	batch, err := c.PrepareBatch(context.Background(), "insert into events")
	if err != nil {
		t.Fatalf("PrepareBatch: %v", err)
	}
	if err := batch.Send(); !errors.Is(err, sendErr) {
		t.Fatalf("Send() = %v, want %v", err, sendErr)
	}
	if err := batch.Abort(); !errors.Is(err, ch.ErrBatchAlreadySent) {
		t.Fatalf("Abort() = %v, want ErrBatchAlreadySent", err)
	}

	span := onlySpan(t, exporter)
	if span.Status.Description != sendErr.Error() {
		t.Errorf("status description = %q, want the send error %q", span.Status.Description, sendErr.Error())
	}
	if n := exceptionCount(span.Events); n != 1 {
		t.Errorf("recorded %d exceptions across Send+Abort, want 1", n)
	}
}

// A failed Send still finalizes the batch driver-side, so IsSent must report
// true or the deferred AbortUnsent fires on a finalized batch.
func TestPrepareBatchIsSentForwardsDriverState(t *testing.T) {
	setupTracing(t)
	sendErr := errors.New("code: 252, TOO_MANY_PARTS")
	c := &Conn{Conn: &fakeConn{batch: &fakeBatch{sendErr: sendErr}}}

	batch, err := c.PrepareBatch(context.Background(), "insert into events")
	if err != nil {
		t.Fatalf("PrepareBatch: %v", err)
	}
	if batch.IsSent() {
		t.Error("IsSent() = true before Send, want false")
	}

	if err := batch.Send(); !errors.Is(err, sendErr) {
		t.Fatalf("Send() = %v, want %v", err, sendErr)
	}
	if !batch.IsSent() {
		t.Error("IsSent() = false after a failed Send; a deferred abort would fire on a finalized batch")
	}
}

func TestAbortUnsentSkipsFinalizedBatch(t *testing.T) {
	setupTracing(t)
	underlying := &fakeBatch{sendErr: errors.New("code: 252, TOO_MANY_PARTS")}
	c := &Conn{Conn: &fakeConn{batch: underlying}}

	batch, err := c.PrepareBatch(context.Background(), "insert into events")
	if err != nil {
		t.Fatalf("PrepareBatch: %v", err)
	}
	if err := batch.Send(); err == nil {
		t.Fatal("Send() = nil, want the send error")
	}

	AbortUnsent(context.Background(), batch)
	if underlying.aborted {
		t.Error("AbortUnsent aborted an already-sent batch, producing a spurious ErrBatchAlreadySent")
	}
}

func TestAbortUnsentFinalizesAbandonedBatch(t *testing.T) {
	exporter := setupTracing(t)
	underlying := &fakeBatch{}
	c := &Conn{Conn: &fakeConn{batch: underlying}}

	batch, err := c.PrepareBatch(context.Background(), "insert into events")
	if err != nil {
		t.Fatalf("PrepareBatch: %v", err)
	}

	AbortUnsent(context.Background(), batch)
	if !underlying.aborted {
		t.Error("AbortUnsent did not abort an unsent batch, leaking a pooled connection")
	}
	if got := len(exporter.GetSpans()); got != 1 {
		t.Errorf("got %d spans after AbortUnsent, want 1", got)
	}
}

// A failed Append leaves the batch unsent, so this is the one path the insert
// workers actually take where the driver's sent flag and the wrapper's
// recorded/ended flags disagree — an IsSent sourced from wrapper state would
// skip the abort here and strand the pooled connection.
func TestAbortUnsentFinalizesAppendFailedBatch(t *testing.T) {
	exporter := setupTracing(t)
	appendErr := errors.New("code: 53, TYPE_MISMATCH")
	underlying := &fakeBatch{appendErr: appendErr}
	c := &Conn{Conn: &fakeConn{batch: underlying}}

	batch, err := c.PrepareBatch(context.Background(), "insert into events")
	if err != nil {
		t.Fatalf("PrepareBatch: %v", err)
	}
	if err := batch.Append("evt"); !errors.Is(err, appendErr) {
		t.Fatalf("Append() = %v, want %v", err, appendErr)
	}
	if batch.IsSent() {
		t.Fatal("IsSent() = true after a failed Append, want false")
	}

	AbortUnsent(context.Background(), batch)
	if !underlying.aborted {
		t.Error("AbortUnsent did not abort after a failed Append, leaking a pooled connection")
	}
	span := onlySpan(t, exporter)
	if n := exceptionCount(span.Events); n != 1 {
		t.Errorf("recorded %d exceptions, want 1 (the Append error)", n)
	}
}

func TestAbortUnsentToleratesNilBatch(t *testing.T) {
	setupTracing(t)
	AbortUnsent(context.Background(), nil)
}

func TestPrepareBatchCloseErrorRecorded(t *testing.T) {
	exporter := setupTracing(t)
	closeErr := errors.New("code: 210, NETWORK_ERROR")
	c := &Conn{Conn: &fakeConn{batch: &fakeBatch{closeErr: closeErr}}}

	batch, err := c.PrepareBatch(context.Background(), "insert into events")
	if err != nil {
		t.Fatalf("PrepareBatch: %v", err)
	}
	if err := batch.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("Close() = %v, want %v", err, closeErr)
	}

	span := onlySpan(t, exporter)
	if span.Status.Code != codes.Error {
		t.Errorf("failed close left span status %v, want Error", span.Status.Code)
	}
	if span.Status.Description != closeErr.Error() {
		t.Errorf("status description = %q, want %q", span.Status.Description, closeErr.Error())
	}
}

func TestPrepareBatchFlushErrorRecordedWithoutEndingSpan(t *testing.T) {
	exporter := setupTracing(t)
	flushErr := errors.New("code: 210, NETWORK_ERROR")
	c := &Conn{Conn: &fakeConn{batch: &fakeBatch{flushErr: flushErr}}}

	batch, err := c.PrepareBatch(context.Background(), "insert into events")
	if err != nil {
		t.Fatalf("PrepareBatch: %v", err)
	}
	if err := batch.Flush(); !errors.Is(err, flushErr) {
		t.Fatalf("Flush() = %v, want %v", err, flushErr)
	}
	if got := len(exporter.GetSpans()); got != 0 {
		t.Fatalf("Flush ended the span, but the batch is still usable: got %d spans, want 0", got)
	}

	if err := batch.Send(); err != nil {
		t.Fatalf("Send: %v", err)
	}
	span := onlySpan(t, exporter)
	if span.Status.Description != flushErr.Error() {
		t.Errorf("a clean Send erased the flush failure: status = %q, want %q", span.Status.Description, flushErr.Error())
	}
}

func TestQueryFormatCloseErrorRecorded(t *testing.T) {
	exporter := setupTracing(t)
	closeErr := errors.New("read on closed response body")
	c := &Conn{Conn: &fakeConn{stream: &fakeStream{Reader: strings.NewReader(""), closeErr: closeErr}}}

	stream, err := c.QueryFormat(context.Background(), "CSV", "select 1")
	if err != nil {
		t.Fatalf("QueryFormat: %v", err)
	}
	if err := stream.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("Close() = %v, want %v", err, closeErr)
	}

	span := onlySpan(t, exporter)
	if span.Status.Code != codes.Error {
		t.Errorf("failed close left span status %v, want Error", span.Status.Code)
	}
}

func TestPrepareBatchEagerErrorEndsSpanAndReturnsNilBatch(t *testing.T) {
	exporter := setupTracing(t)
	prepErr := errors.New("code: 60, UNKNOWN_TABLE")
	c := &Conn{Conn: &fakeConn{err: prepErr}}

	batch, err := c.PrepareBatch(context.Background(), "insert into nope")
	if !errors.Is(err, prepErr) {
		t.Fatalf("err = %v, want %v", err, prepErr)
	}
	if batch != nil {
		t.Errorf("batch = %v, want nil on error", batch)
	}

	span := onlySpan(t, exporter)
	if span.Status.Code != codes.Error {
		t.Errorf("span status = %v, want Error", span.Status.Code)
	}
}
