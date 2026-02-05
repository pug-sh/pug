package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/fivebitsio/cotton/internal/app/server"
	"github.com/joho/godotenv"
)

func main() {
	ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer done()

	if err := godotenv.Load(); err != nil {
		slog.Error("error loading .env file", slog.Any("err", err))
		os.Exit(1)
	}

	if err := server.Run(ctx); err != nil {
		slog.Error("server error", slog.Any("err", err))
		os.Exit(1)
	}
}
