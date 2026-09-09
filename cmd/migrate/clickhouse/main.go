package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/pug-sh/pug/internal/app/migrate/clickhouse"
	"github.com/pug-sh/pug/internal/dotenv"
	"github.com/pug-sh/pug/internal/slogx"
)

func main() {
	ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer done()

	dotenv.LoadOrExit(ctx)

	if err := clickhouse.Up(ctx, 0); err != nil {
		slog.ErrorContext(ctx, "clickhouse migration error", slogx.Error(err))
		os.Exit(1)
	}
}
