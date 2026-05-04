package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/pug-sh/pug/internal/app/migrate/nats"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/joho/godotenv"
)

func main() {
	ctx := context.Background()

	if err := godotenv.Load(); err != nil {
		slog.DebugContext(ctx, "No .env file found")
	}

	if err := nats.Run(ctx); err != nil {
		slog.ErrorContext(ctx, "NATS initialization error", slogx.Error(err))
		os.Exit(1)
	}
}
