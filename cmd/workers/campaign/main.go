package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/pug-sh/pug/internal/app/workers/campaigns"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/joho/godotenv"
)

func main() {
	ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer done()

	if err := godotenv.Load(); err != nil {
		slog.DebugContext(ctx, "No .env file found, relying on environment variables")
	}

	if err := campaigns.Run(ctx); err != nil {
		slog.ErrorContext(ctx, "error starting campaign worker", slogx.Error(err))
		os.Exit(1)
	}
}
