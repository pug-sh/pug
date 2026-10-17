package server

import (
	"errors"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestNewDepsFailsWithoutPostgres(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	t.Setenv("PUG_JWT_SECRET_KEY", "test")
	t.Setenv("DATABASE_URL", "postgres://pug@127.0.0.1:1/pug")
	for _, k := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"} {
		t.Setenv(k, "")
	}

	if _, err := newDeps(t.Context()); !errors.As(err, new(*pgconn.ConnectError)) {
		t.Fatalf("newDeps = %v, want a Postgres connect error", err)
	}
}
