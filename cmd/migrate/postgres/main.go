package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/fivebitsio/cotton/internal/app/migrate/postgres"
	"github.com/joho/godotenv"
)

func main() {
	ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer done()

	if err := godotenv.Load(); err != nil {
		slog.DebugContext(ctx,"No .env file found, relying on environment variables")
	}

	if err := postgres.Up(ctx, 0); err != nil {
		slog.ErrorContext(ctx,"postgres migration error", slog.Any("err", err))
		os.Exit(1)
	}
}
