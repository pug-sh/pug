package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/fivebitsio/cotton/internal/app/workers/events"
	"github.com/fivebitsio/cotton/internal/deps/telemetry"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/joho/godotenv"
)

func main() {
	ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer done()

	if err := godotenv.Load(); err != nil {
		slog.DebugContext(ctx, "No .env file found, relying on environment variables")
	}

	closeOtel, err := telemetry.SetupSDK(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "failed to setup telemetry", slogx.Error(err))
		os.Exit(1)
	}
	defer func() {
		if err := closeOtel(ctx); err != nil {
			slog.ErrorContext(ctx, "failed to shutdown telemetry", slogx.Error(err))
		}
	}()

	if err := events.Run(ctx); err != nil {
		slog.ErrorContext(ctx, "error starting events worker", slogx.Error(err))
		os.Exit(1)
	}
}
