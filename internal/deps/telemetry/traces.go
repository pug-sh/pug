package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/trace"
)

func newTracesExporter(ctx context.Context) (trace.SpanExporter, error) {
	var opts []otlptracegrpc.Option
	if insecureExporter() {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	return otlptracegrpc.New(ctx, opts...)
}

func newTracesProvider(ctx context.Context) (*trace.TracerProvider, error) {
	traceExporter, err := newTracesExporter(ctx)
	if err != nil {
		return nil, err
	}

	traceProvider := trace.NewTracerProvider(trace.WithBatcher(traceExporter))
	return traceProvider, nil
}
