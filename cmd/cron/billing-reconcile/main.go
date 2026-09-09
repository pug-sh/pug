package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	billingcron "github.com/pug-sh/pug/internal/app/cron/billing"
	"github.com/pug-sh/pug/internal/dotenv"
	"github.com/pug-sh/pug/internal/slogx"
)

func main() {
	ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer done()

	dotenv.LoadOrExit(ctx)

	// Run has already logged the failure through the OTLP handler; this line goes
	// to stderr after telemetry shut down, so it says only what main adds.
	if err := billingcron.Run(ctx); err != nil {
		slog.ErrorContext(ctx, "exiting non-zero", slogx.Error(err))
		os.Exit(1)
	}
}
