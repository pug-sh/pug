package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// RecordError records err on the current span from ctx, sets the span status to
// Error, and captures a stack trace on the error event. Any additional attributes
// are also attached to the error event. It is a no-op when err is nil; when ctx
// carries no active span, the OTel SDK's no-op span silently discards the calls.
// Callers that need errors to always be recorded should also use slog.ErrorContext.
func RecordError(ctx context.Context, err error, attributes ...attribute.KeyValue) {
	RecordErrorOnSpan(trace.SpanFromContext(ctx), err, attributes...)
}

// RecordErrorOnSpan is RecordError against a span held directly. Use it where a
// span outlives the call that started it, so the originating context does not
// have to be retained purely to find the span again.
func RecordErrorOnSpan(span trace.Span, err error, attributes ...attribute.KeyValue) {
	if err == nil {
		return
	}
	span.RecordError(err, trace.WithStackTrace(true), trace.WithAttributes(attributes...))
	span.SetStatus(codes.Error, err.Error())
}
