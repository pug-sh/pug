package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	emailworker "github.com/pug-sh/pug/internal/app/workers/email"
	"github.com/pug-sh/pug/internal/dotenv"
	"github.com/pug-sh/pug/internal/slogx"
)

func main() {
	ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer done()

	dotenv.LoadOrExit(ctx)

	if err := emailworker.Run(ctx); err != nil {
		slog.ErrorContext(ctx, "error starting email worker", slogx.Error(err))
		os.Exit(1)
	}
}
