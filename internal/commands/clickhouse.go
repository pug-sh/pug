package commands

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/fivebitsio/cotton/pkg/clickhouse"
	"github.com/fivebitsio/cotton/pkg/logger"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/clickhouse"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/joho/godotenv"
	"github.com/sethvargo/go-envconfig"
	"github.com/spf13/cobra"
)

type clickhouseMigrateDeps struct {
	Migration *migrate.Migrate
}

func (deps *clickhouseMigrateDeps) Close(ctx context.Context) error {
	deps.Migration.Close()
	return nil
}

func newClickhouseMigrateDeps(ctx context.Context) (*clickhouseMigrateDeps, error) {
	var databaseConfig clickhouse.Config

	if err := envconfig.Process(ctx, &databaseConfig); err != nil {
		logger.Log.Error("error loading clickhouse config", slog.Any("err", err))
		os.Exit(1)
	}

	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	absPath := filepath.Join(wd, "schema", "clickhouse", "migrations")

	absPath, err = filepath.Abs(absPath)
	if err != nil {
		return nil, err
	}

	migration, err := migrate.New(
		"file://"+absPath,
		databaseConfig.DSN(),
	)
	if err != nil {
		return nil, err
	}

	return &clickhouseMigrateDeps{
		Migration: migration,
	}, nil
}

var ClickhouseMigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Run database migrations for clickhouse",
	Long:  `Run database migrations for clickhouse by applying migration files.`,
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer done()
		if err := godotenv.Load(); err != nil {
			logger.Log.Error("error loading config", slog.Any("err", err))
			os.Exit(1)
		}

		direction, _ := cmd.Flags().GetString("direction")
		numOfMigrations, _ := cmd.Flags().GetInt("num")

		deps, err := newClickhouseMigrateDeps(ctx)

		if err != nil {
			logger.Log.Error("error while initializing clickhouse dependencies", slog.Any("err", err))
			os.Exit(1)
		}

		defer func() {
			done()
			deps.Close(ctx)
		}()

		switch direction {
		case "up":
			if err := runClickhouseMigrationsUp(deps.Migration, numOfMigrations); err != nil {
				logger.Log.Error("Error while applying clickhouse migrations", slog.Any("err", err))
				os.Exit(1)
			}
		case "down":
			if err := runClickhouseMigrationsDown(deps.Migration, numOfMigrations); err != nil {
				logger.Log.Error("Error while rolling back clickhouse migrations", slog.Any("err", err))
				os.Exit(1)
			}
		}
	},
}

func runClickhouseMigrationsUp(migration *migrate.Migrate, num int) error {
	if num == 0 {
		if err := migration.Up(); err != nil && err != migrate.ErrNoChange {
			return err
		}
		logger.Log.Info("applied all clickhouse migrations")
		return nil
	}

	if err := migration.Steps(num); err != nil && err != migrate.ErrNoChange {
		return err
	}

	logger.Log.Info("applied clickhouse migrations", slog.Int("appliedMigrations", num))
	return nil
}

func runClickhouseMigrationsDown(migration *migrate.Migrate, num int) error {
	if num == 0 {
		if err := migration.Down(); err != nil && err != migrate.ErrNoChange {
			return err
		}
		logger.Log.Info("rolled back all clickhouse migrations")
		return nil
	}

	if err := migration.Steps(-num); err != nil && err != migrate.ErrNoChange {
		return err
	}

	logger.Log.Info("rolled back clickhouse migrations", slog.Int("rolledBackMigrations", num))
	return nil
}

func init() {
	ClickhouseMigrateCmd.Flags().StringP("direction", "d", "up", "can be any of 'up' or 'down' (default: up)")
	ClickhouseMigrateCmd.Flags().IntP("num", "n", 0, "number of migrations to apply")
}
