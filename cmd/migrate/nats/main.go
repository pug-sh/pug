package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/fivebitsio/cotton/internal/app/migrate/nats"
	"github.com/joho/godotenv"
)

func main() {
	ctx := context.Background()

	if err := godotenv.Load(); err != nil {
		slog.Debug("No .env file found")
	}

	if err := nats.Run(ctx); err != nil {
		slog.Error("NATS initialization error", slog.Any("err", err))
		os.Exit(1)
	}
}
