package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"
	usagecron "github.com/pug-sh/pug/internal/app/cron/usage"
	"github.com/pug-sh/pug/internal/slogx"
)

func main() {
	ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer done()

	if err := godotenv.Load(); err != nil {
		slog.DebugContext(ctx, "No .env file found, relying on environment variables")
	}

	if err := usagecron.Run(ctx); err != nil {
		slog.ErrorContext(ctx, "usage metering pass failed", slogx.Error(err))
		os.Exit(1)
	}
}
