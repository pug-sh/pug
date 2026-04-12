package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/trace"
)

func newTracesExporter(ctx context.Context) (trace.SpanExporter, error) {
	return otlptracegrpc.New(ctx, otlptracegrpc.WithInsecure())
}

func newTracesProvider(ctx context.Context) (*trace.TracerProvider, error) {
	traceExporter, err := newTracesExporter(ctx)
	if err != nil {
		return nil, err
	}

	traceProvider := trace.NewTracerProvider(trace.WithBatcher(traceExporter))
	return traceProvider, nil
}
