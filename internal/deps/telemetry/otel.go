package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"connectrpc.com/otelconnect"
	"github.com/fivebitsio/cotton/internal/slogx"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
)

// SetupSDK bootstraps the OpenTelemetry pipeline (propagator, tracer, meter, and
// logger providers). On success it returns a cleanup function that must be called
// to flush and shut down all providers.
func SetupSDK(ctx context.Context) (func(context.Context) error, error) {
	var shutdowns []func(context.Context) error
	success := false
	defer func() {
		if !success {
			for i := len(shutdowns) - 1; i >= 0; i-- {
				if err := shutdowns[i](ctx); err != nil {
					slog.ErrorContext(ctx, "cleanup error during init rollback", slogx.Error(err))
				}
			}
		}
	}()

	otel.SetTextMapPropagator(newPropagator())

	tracerProvider, err := newTracesProvider(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "unable to create tracer provider", slogx.Error(err))
		return nil, err
	}
	shutdowns = append(shutdowns, tracerProvider.Shutdown)

	meterProvider, err := newMetricsProvider(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "unable to create meter provider", slogx.Error(err))
		return nil, err
	}
	shutdowns = append(shutdowns, meterProvider.Shutdown)

	loggerProvider, err := newLoggerProvider(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "unable to create logger provider", slogx.Error(err))
		return nil, err
	}

	// Set globals now that all providers initialized successfully.
	otel.SetTracerProvider(tracerProvider)
	otel.SetMeterProvider(meterProvider)
	global.SetLoggerProvider(loggerProvider)

	serviceName := os.Getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		slog.WarnContext(ctx, "OTEL_SERVICE_NAME is not set; traces, metrics, and logs will lack a service name identifier")
	}
	slog.SetDefault(otelslog.NewLogger(serviceName, otelslog.WithLoggerProvider(loggerProvider), otelslog.WithSource(true)))

	shutdown := func(ctx context.Context) error {
		var errs []error
		if err := tracerProvider.Shutdown(ctx); err != nil {
			slog.ErrorContext(ctx, "failed to shutdown tracer provider", slogx.Error(err))
			errs = append(errs, err)
		}
		if err := meterProvider.Shutdown(ctx); err != nil {
			slog.ErrorContext(ctx, "failed to shutdown meter provider", slogx.Error(err))
			errs = append(errs, err)
		}
		// Restore a plain stderr logger before shutting down the OTel logger
		// provider so its own shutdown error can still be logged.
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
		if err := loggerProvider.Shutdown(ctx); err != nil {
			slog.ErrorContext(ctx, "failed to shutdown logger provider", slogx.Error(err))
			errs = append(errs, err)
		}
		return errors.Join(errs...)
	}

	success = true
	return shutdown, nil
}

// NewOtelInterceptor initializes the OpenTelemetry SDK and returns a Connect RPC
// interceptor for automatic span creation, along with a cleanup function that
// must be called on shutdown.
func NewOtelInterceptor(ctx context.Context) (*otelconnect.Interceptor, func(context.Context) error, error) {
	shutdown, err := SetupSDK(ctx)
	if err != nil {
		return nil, nil, err
	}

	otelInterceptor, err := otelconnect.NewInterceptor()
	if err != nil {
		slog.ErrorContext(ctx, "failed to create otel interceptor", slogx.Error(err))
		shutdown(ctx)
		return nil, nil, err
	}

	return otelInterceptor, shutdown, nil
}

func newPropagator() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
}
