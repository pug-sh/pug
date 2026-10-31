package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	expiredtokenscron "github.com/pug-sh/pug/internal/app/cron/expiredtokens"
	"github.com/pug-sh/pug/internal/dotenv"
	"github.com/pug-sh/pug/internal/slogx"
)

func main() {
	ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer done()

	dotenv.LoadOrExit(ctx)

	if err := expiredtokenscron.Run(ctx); err != nil {
		slog.ErrorContext(ctx, "exiting non-zero", slogx.Error(err))
		os.Exit(1)
	}
}
