package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/pug-sh/pug/internal/app/workers/profiles/identify"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/joho/godotenv"
)

func main() {
	ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer done()

	if err := godotenv.Load(); err != nil {
		slog.DebugContext(ctx, "No .env file found, relying on environment variables")
	}

	if err := identify.Run(ctx); err != nil {
		slog.ErrorContext(ctx, "error starting profile identify worker", slogx.Error(err))
		os.Exit(1)
	}
}
