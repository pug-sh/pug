package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"connectrpc.com/otelconnect"
	"github.com/fivebitsio/cotton/internal/slogx"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
)

const shutdownTimeout = 5 * time.Second

var (
	setupOnce   sync.Once
	setupResult func(context.Context) error
	setupErr    error
)

// SetupSDK bootstraps the OpenTelemetry pipeline (propagator, tracer, meter, and
// logger providers). It is safe to call from multiple goroutines; only the first
// call initializes the SDK. Subsequent calls return the same shutdown function.
// The returned shutdown is idempotent — only the first call performs the actual
// shutdown; subsequent calls are no-ops that return nil.
func SetupSDK(ctx context.Context) (func(context.Context) error, error) {
	setupOnce.Do(func() {
		setupResult, setupErr = doSetupSDK(ctx)
	})
	return setupResult, setupErr
}

func doSetupSDK(ctx context.Context) (func(context.Context) error, error) {
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
	shutdowns = append(shutdowns, loggerProvider.Shutdown)

	// Set globals now that all providers initialized successfully.
	otel.SetTracerProvider(tracerProvider)
	otel.SetMeterProvider(meterProvider)
	global.SetLoggerProvider(loggerProvider)

	serviceName := os.Getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		slog.WarnContext(ctx, "OTEL_SERVICE_NAME is not set; traces, metrics, and logs will lack a service name identifier")
	}
	slog.SetDefault(otelslog.NewLogger(serviceName, otelslog.WithLoggerProvider(loggerProvider), otelslog.WithSource(true)))

	var shutdownOnce sync.Once
	var shutdownErr error
	shutdown := func(ctx context.Context) error {
		shutdownOnce.Do(func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
			defer cancel()
			var errs []error
			if err := tracerProvider.Shutdown(shutdownCtx); err != nil {
				slog.ErrorContext(shutdownCtx, "failed to shutdown tracer provider", slogx.Error(err))
				errs = append(errs, err)
			}
			if err := meterProvider.Shutdown(shutdownCtx); err != nil {
				slog.ErrorContext(shutdownCtx, "failed to shutdown meter provider", slogx.Error(err))
				errs = append(errs, err)
			}
			// Restore a plain stderr logger before shutting down the OTel logger
			// provider so its own shutdown error can still be logged.
			slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
			if err := loggerProvider.Shutdown(shutdownCtx); err != nil {
				slog.ErrorContext(shutdownCtx, "failed to shutdown logger provider", slogx.Error(err))
				errs = append(errs, err)
			}
			shutdownErr = errors.Join(errs...)
		})
		return shutdownErr
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
		if shutdownErr := shutdown(ctx); shutdownErr != nil {
			slog.ErrorContext(ctx, "failed to shutdown otel after interceptor failure", slogx.Error(shutdownErr))
		}
		return nil, nil, err
	}

	return otelInterceptor, shutdown, nil
}

// insecureExporter returns true when OTEL_EXPORTER_OTLP_INSECURE is "true" or
// unset. The OTel SDK's gRPC exporters default to TLS; this helper defaults to
// insecure for local development while allowing production deployments to enable
// TLS by setting the env var to "false".
func insecureExporter() bool {
	v := os.Getenv("OTEL_EXPORTER_OTLP_INSECURE")
	return v == "" || v == "true"
}

func newPropagator() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
}
