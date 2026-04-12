package telemetry

import (
	"context"
	"log/slog"
	"os"

	"connectrpc.com/otelconnect"
	"github.com/fivebitsio/cotton/internal/slogx"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
)

// SetupSDK bootstraps the OpenTelemetry pipeline.
// If it does not return an error, make sure to call shutdown for proper cleanup.
func SetupSDK(ctx context.Context) (func(context.Context), error) {
	// Set up propagator.
	prop := newPropagator()
	otel.SetTextMapPropagator(prop)

	// Set up trace provider.
	tracerProvider, err := newTracesProvider(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "unable to create tracer provider", slogx.Error(err))
		return nil, err
	}

	otel.SetTracerProvider(tracerProvider)

	// Set up meter provider.
	meterProvider, err := newMetricsProvider(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "unable to create meter provider", slogx.Error(err))
		return nil, err
	}
	otel.SetMeterProvider(meterProvider)

	// Set up logger provider.
	loggerProvider, err := newLoggerProvider(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "unable to create logger provider", slogx.Error(err))
		return nil, err
	}

	serviceName := os.Getenv("OTEL_SERVICE_NAME")
	otelLog := otelslog.NewLogger(serviceName, otelslog.WithLoggerProvider(loggerProvider), otelslog.WithSource(true))
	slog.SetDefault(otelLog)
	global.SetLoggerProvider(loggerProvider)

	close := func(ctx context.Context) {
		tracerProvider.Shutdown(ctx)
		meterProvider.Shutdown(ctx)
		loggerProvider.Shutdown(ctx)
	}

	return close, nil
}

func NewOtelInterceptor(ctx context.Context) (*otelconnect.Interceptor, func(context.Context), error) {
	close, err := SetupSDK(ctx)
	if err != nil {
		return nil, nil, err
	}

	otelInterceptor, err := otelconnect.NewInterceptor()
	if err != nil {
		slog.ErrorContext(ctx, "failed to create otel interceptor", slogx.Error(err))
		return nil, nil, err
	}

	return otelInterceptor, close, nil
}

func newPropagator() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
}
