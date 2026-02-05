package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/fivebitsio/cotton/internal/app/workers/subscriptions"
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

	if err := subscriptions.Run(ctx); err != nil {
		logger.Log.Error("error starting subscription worker", slog.Any("err", err))
		os.Exit(1)
	}
}
