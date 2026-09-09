package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/pug-sh/pug/internal/app/workers/compliance"
	"github.com/pug-sh/pug/internal/dotenv"
	"github.com/pug-sh/pug/internal/slogx"
)

func main() {
	ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer done()

	dotenv.LoadOrExit(ctx)

	if err := compliance.Run(ctx); err != nil {
		slog.ErrorContext(ctx, "error starting compliance worker", slogx.Error(err))
		os.Exit(1)
	}
}
