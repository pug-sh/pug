package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"
	// "github.com/pug-sh/pug/internal/app/workers/scheduler"
	// "github.com/pug-sh/pug/internal/slogx"
)

func main() {
	ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer done()

	if err := godotenv.Load(); err != nil {
		slog.DebugContext(ctx, "No .env file found, relying on environment variables")
	}

	// if err := scheduler.Run(ctx); err != nil {
	// 	slog.ErrorContext(ctx, "error starting scheduler worker", slogx.Error(err))
	// 	os.Exit(1)
	// }
	slog.InfoContext(ctx, "scheduler worker is disabled")
	os.Exit(0)
}
