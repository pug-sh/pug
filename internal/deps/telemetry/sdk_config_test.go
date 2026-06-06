package telemetry

import (
	"testing"
)

const (
	envOTLPEndpoint        = "OTEL_EXPORTER_OTLP_ENDPOINT"
	envOTLPTracesEndpoint  = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	envOTLPMetricsEndpoint = "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"
	envOTLPLogsEndpoint    = "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"
)

// clearOTLPEndpointEnv makes the test hermetic regardless of the ambient
// environment by clearing every endpoint var the detector consults.
func clearOTLPEndpointEnv(t *testing.T) {
	t.Helper()
	for _, v := range []string{envOTLPEndpoint, envOTLPTracesEndpoint, envOTLPMetricsEndpoint, envOTLPLogsEndpoint} {
		t.Setenv(v, "")
	}
}

func TestResolveOtelMode(t *testing.T) {
	tests := []struct {
		name string
		set  map[string]string
		want string
	}{
		{name: "no endpoint configured -> stdout", set: nil, want: "stdout"},
		{name: "general endpoint -> otlp", set: map[string]string{envOTLPEndpoint: "localhost:4317"}, want: "otlp"},
		{name: "traces endpoint only -> otlp", set: map[string]string{envOTLPTracesEndpoint: "collector:4317"}, want: "otlp"},
		{name: "metrics endpoint only -> otlp", set: map[string]string{envOTLPMetricsEndpoint: "collector:4317"}, want: "otlp"},
		{name: "logs endpoint only -> otlp", set: map[string]string{envOTLPLogsEndpoint: "collector:4317"}, want: "otlp"},
		{name: "whitespace-only endpoint -> stdout", set: map[string]string{envOTLPEndpoint: "   "}, want: "stdout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearOTLPEndpointEnv(t)
			for k, v := range tt.set {
				t.Setenv(k, v)
			}
			if got := resolveOtelMode(); got != tt.want {
				t.Fatalf("resolveOtelMode() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestResolveOtelModeIgnoresPugOtel locks in the removal of the PUG_OTEL switch:
// mode selection must derive solely from the standard OTLP endpoint env vars.
func TestResolveOtelModeIgnoresPugOtel(t *testing.T) {
	clearOTLPEndpointEnv(t)
	t.Setenv(envOTLPEndpoint, "localhost:4317")
	t.Setenv("PUG_OTEL", "stdout")
	if got := resolveOtelMode(); got != "otlp" {
		t.Fatalf("resolveOtelMode() = %q, want otlp (PUG_OTEL must be ignored)", got)
	}
}
