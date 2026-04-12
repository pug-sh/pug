package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// RecordError records err on the current span from ctx, sets the span status to
// Error, and attaches a stack trace. Additional attributes are attached to the
// error event. It is a no-op if err is nil or if ctx has no active span.
// Callers that need errors to always be recorded should also use slog.ErrorContext.
func RecordError(ctx context.Context, err error, attributes ...attribute.KeyValue) {
	if err == nil {
		return
	}
	span := trace.SpanFromContext(ctx)
	span.RecordError(err, trace.WithStackTrace(true), trace.WithAttributes(attributes...))
	span.SetStatus(codes.Error, err.Error())
}
