package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/fivebitsio/cotton/pkg/logger"
	"github.com/fivebitsio/cotton/pkg/postgres"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/joho/godotenv"
	migrate "github.com/rubenv/sql-migrate"
	"github.com/sethvargo/go-envconfig"
)

type Dependencies struct {
	DB *postgres.DB
}

func (deps *Dependencies) Close(ctx context.Context) error {
	deps.DB.Close(ctx)

	return nil
}

func newDependencies(ctx context.Context) (*Dependencies, error) {
	var databaseConfig postgres.Config

	if err := envconfig.Process(ctx, &databaseConfig); err != nil {
		logger.Log.Error("error loading database config", slog.Any("err", err))
		os.Exit(1)
	}

	db, err := postgres.NewFromConfig(ctx, &databaseConfig)

	if err != nil {
		return nil, err
	}

	return &Dependencies{
		DB: db,
	}, nil
}

func main() {
	ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	if err := godotenv.Load(); err != nil {
		logger.Log.Error("error loading config", slog.Any("err", err))
		os.Exit(1)
	}

	direction := flag.String("direction", "up", "can be any of 'up' or 'down'")
	numOfMigrations := flag.Int("num", 0, "number of migrations to apply")

	flag.Parse()

	migrations := &migrate.FileMigrationSource{
		Dir: "schema/postgres/migrations",
	}

	deps, err := newDependencies(ctx)

	if err != nil {
		logger.Log.Error("error while initializing dependencies", slog.Any("err", err))
		os.Exit(1)
	}

	defer func() {
		done()
		deps.Close(ctx)
	}()

	switch *direction {
	case "up":
		appliedMigrations, err := migrate.ExecMax(
			stdlib.OpenDBFromPool(deps.DB.Pool),
			"postgres",
			migrations,
			migrate.Up,
			*numOfMigrations,
		)

		if err != nil {
			logger.Log.Error(
				"Error while applying migrations",
				slog.Any("err", err))
		}

		logger.Log.Info("applied migrations", slog.Int("appliedMigrations", appliedMigrations))

	case "down":
		appliedMigrations, err := migrate.ExecMax(
			stdlib.OpenDBFromPool(deps.DB.Pool),
			"postgres",
			migrations,
			migrate.Down,
			*numOfMigrations,
		)

		if err != nil {
			logger.Log.Error(
				"Error while applying migrations",
				slog.Any("err", err))
		}

		logger.Log.Info("applied migrations", slog.Int("appliedMigrations", appliedMigrations))
	}
}
