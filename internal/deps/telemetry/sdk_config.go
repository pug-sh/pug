package telemetry

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/log/global"
	lognoop "go.opentelemetry.io/otel/log/noop"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// otlpEndpointEnvVars are the standard OpenTelemetry environment variables that
// point the SDK's exporters at a collector. Their presence is how pug decides
// whether to export via OTLP; with none set, telemetry falls back to stdout.
var otlpEndpointEnvVars = []string{
	"OTEL_EXPORTER_OTLP_ENDPOINT",
	"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
	"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
	"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
}

// otlpConfigured reports whether the environment points the OTel SDK at a
// collector — i.e. whether OTLP export is wanted. It is true when any
// otlpEndpointEnvVars entry holds a non-empty value; a present-but-blank var
// (e.g. OTEL_EXPORTER_OTLP_ENDPOINT=) is treated as unset so a conditionally
// templated empty value can't flip pug into exporting at a collector that
// isn't there.
func otlpConfigured() bool {
	for _, name := range otlpEndpointEnvVars {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return true
		}
	}
	return false
}

// resolveOtelMode selects the telemetry export mode from the environment with no
// pug-specific switch: "otlp" when an OTLP endpoint is configured, "stdout"
// otherwise.
func resolveOtelMode() string {
	if otlpConfigured() {
		return "otlp"
	}
	return "stdout"
}

func doSetupWithoutExport(ctx context.Context) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(newPropagator())
	otel.SetTracerProvider(tracenoop.NewTracerProvider())
	otel.SetMeterProvider(metricnoop.NewMeterProvider())
	global.SetLoggerProvider(lognoop.NewLoggerProvider())

	installStdoutLogHandler()
	slog.InfoContext(ctx, "no OTLP endpoint configured; application logs on stdout (OTLP export off)")

	return func(context.Context) error { return nil }, nil
}

func installStdoutLogHandler() {
	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{AddSource: true})
	slog.SetDefault(slog.New(newCorrelationHandler(handler)))
}
