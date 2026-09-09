// Package dotenv loads the optional .env of a local checkout. Deployments ship
// none and set real environment variables.
package dotenv

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"

	"github.com/joho/godotenv"
	"github.com/pug-sh/pug/internal/slogx"
)

// Load applies .env without overriding the environment. godotenv reports a line
// with no name as success and discards os.Setenv errors, so both are caught here.
func Load() error {
	env, err := godotenv.Read()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("load .env: %w", err)
	}
	if value, ok := env[""]; ok {
		return fmt.Errorf("load .env: %q has no name", value)
	}
	for name, value := range env {
		if _, ok := os.LookupEnv(name); ok {
			continue
		}
		if err := os.Setenv(name, value); err != nil {
			return fmt.Errorf("load .env: %s: %w", name, err)
		}
	}
	return nil
}

// LoadOrExit logs and exits 1 when Load fails.
func LoadOrExit(ctx context.Context) {
	if err := Load(); err != nil {
		slog.ErrorContext(ctx, "failed to load .env", slogx.Error(err)) // puglint:exempt — no span at startup
		os.Exit(1)
	}
}
