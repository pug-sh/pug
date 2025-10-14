package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/fivebitsio/cotton/pkg/logger"
	"github.com/joho/godotenv"
)

func main() {
	ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer done()

	if err := godotenv.Load(); err != nil {
		logger.Log.Error("error loading .env file", slog.Any("err", err))
		os.Exit(1)
	}

	deps, err := newDependencies(ctx)
	if err != nil {
		logger.Log.Error("error while initializing dependencies", slog.Any("err", err))
		os.Exit(1)
	}
	defer func() {
		if deps.pgRo != nil {
			deps.pgRo.Close()
		}
		if deps.pgW != nil {
			deps.pgW.Close()
		}
	}()

	// TODO: Set up your server here using deps.pgRo (read operations) and deps.pgW (write operations)
	logger.Log.Info("Server started successfully with database connections")
}
