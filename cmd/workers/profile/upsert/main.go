package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"
	"github.com/pug-sh/pug/internal/app/workers/profiles/upsert"
	"github.com/pug-sh/pug/internal/slogx"
)

func main() {
	ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer done()

	if err := godotenv.Load(); err != nil {
		slog.DebugContext(ctx, "No .env file found, relying on environment variables")
	}

	if err := upsert.Run(ctx); err != nil {
		slog.ErrorContext(ctx, "error starting profile upsert worker", slogx.Error(err))
		os.Exit(1)
	}
}
