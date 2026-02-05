package logger

import (
	"context"
	"log/slog"
	"os"

	"github.com/sethvargo/go-envconfig"
)

type contextKey string

const loggerKey = contextKey("logger")

var (
	Log = NewLogger()
)

func NewLogger() *slog.Logger {
	logginConfig := Config{}
	if err := envconfig.Process(context.Background(), &logginConfig); err != nil {
		slog.Error("Error loading logger config", slog.Any("err", err)) // slog does not have an error type handler use slog.Any
		os.Exit(1)                                                      // slog does not have a fatal function - use os.Exit(1) everywhere
	}
	handler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logginConfig.toSlogLevel()})
	return slog.New(handler)
}

func AddLoggerToContext(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, logger)
}

func FromContext(ctx context.Context) *slog.Logger {
	if logger, ok := ctx.Value(loggerKey).(*slog.Logger); ok {
		return logger
	}
	return Log
}
