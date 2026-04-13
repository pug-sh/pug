package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/sdk/metric"
)

func newMetricsExporter(ctx context.Context) (metric.Exporter, error) {
	var opts []otlpmetricgrpc.Option
	if insecureExporter() {
		opts = append(opts, otlpmetricgrpc.WithInsecure())
	}
	return otlpmetricgrpc.New(ctx, opts...)
}

func newMetricsProvider(ctx context.Context) (*metric.MeterProvider, error) {
	metricExporter, err := newMetricsExporter(ctx)
	if err != nil {
		return nil, err
	}

	meterProvider := metric.NewMeterProvider(metric.WithReader(metric.NewPeriodicReader(metricExporter)))
	return meterProvider, nil
}
