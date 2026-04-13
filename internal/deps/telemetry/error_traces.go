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
	if err == nil {
		return
	}
	span := trace.SpanFromContext(ctx)
	span.RecordError(err, trace.WithStackTrace(true), trace.WithAttributes(attributes...))
	span.SetStatus(codes.Error, err.Error())
}
