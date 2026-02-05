package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/fivebitsio/cotton/internal/app/migrate/postgres"
	"github.com/fivebitsio/cotton/internal/deps/logger"
	"github.com/joho/godotenv"
)

func main() {
	ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer done()

	if err := godotenv.Load(); err != nil {
		logger.Log.Error("error loading .env file", slog.Any("err", err))
		os.Exit(1)
	}

	if err := postgres.Up(ctx, 0); err != nil {
		logger.Log.Error("postgres migration error", slog.Any("err", err))
		os.Exit(1)
	}
}
