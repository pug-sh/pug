package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	lognoop "go.opentelemetry.io/otel/log/noop"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/log/global"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// parseOtelMode reads PUG_OTEL. Unset or "otlp" selects OTLP export; "stdout"
// skips export and writes application logs to stdout. Returns an error for any other value.
func parseOtelMode() (string, error) {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("PUG_OTEL")))
	switch raw {
	case "", "otlp":
		return "otlp", nil
	case "stdout":
		return "stdout", nil
	default:
		return "", fmt.Errorf("invalid PUG_OTEL %q: want otlp or stdout", os.Getenv("PUG_OTEL"))
	}
}

func doSetupWithoutExport(ctx context.Context) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(newPropagator())
	otel.SetTracerProvider(tracenoop.NewTracerProvider())
	otel.SetMeterProvider(metricnoop.NewMeterProvider())
	global.SetLoggerProvider(lognoop.NewLoggerProvider())

	installStdoutLogHandler()
	slog.InfoContext(ctx, "PUG_OTEL=stdout; application logs on stdout (OTLP export off)")

	return func(context.Context) error { return nil }, nil
}

func installStdoutLogHandler() {
	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{AddSource: true})
	slog.SetDefault(slog.New(newCorrelationHandler(handler)))
}

// ErrInvalidOtelMode is returned by SetupSDK when PUG_OTEL is set to an unsupported value.
var ErrInvalidOtelMode = errors.New("invalid PUG_OTEL")
