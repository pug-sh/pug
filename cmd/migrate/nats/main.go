package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/pug-sh/pug/internal/app/migrate/nats"
	"github.com/pug-sh/pug/internal/dotenv"
	"github.com/pug-sh/pug/internal/slogx"
)

func main() {
	ctx := context.Background()

	dotenv.LoadOrExit(ctx)

	if err := nats.Run(ctx); err != nil {
		slog.ErrorContext(ctx, "NATS initialization error", slogx.Error(err))
		os.Exit(1)
	}
}
