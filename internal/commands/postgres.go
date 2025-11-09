package commands

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/fivebitsio/cotton/pkg/logger"
	"github.com/fivebitsio/cotton/pkg/postgres"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/joho/godotenv"
	"github.com/sethvargo/go-envconfig"
	"github.com/spf13/cobra"
)

type migrateDeps struct {
	DB        *postgres.DB
	Migration *migrate.Migrate
}

func (deps *migrateDeps) Close(ctx context.Context) error {
	deps.Migration.Close()
	deps.DB.Close(ctx)
	return nil
}

func newMigrateDeps(ctx context.Context) (*migrateDeps, error) {
	var databaseConfig postgres.Config

	if err := envconfig.Process(ctx, &databaseConfig); err != nil {
		logger.Log.Error("error loading database config", slog.Any("err", err))
		os.Exit(1)
	}

	db, err := postgres.NewFromConfig(ctx, &databaseConfig)

	if err != nil {
		return nil, err
	}

	wd, _ := os.Getwd()
	absPath := filepath.Join(wd, "schema", "postgres", "migrations")

	migration, err := migrate.New(
		"file://"+absPath,
		databaseConfig.ConnectionString(),
	)
	if err != nil {
		return nil, err
	}

	return &migrateDeps{
		DB:        db,
		Migration: migration,
	}, nil
}

var MigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Run database migrations for postgres",
	Long:  `Run database migrations for postgres by applying migration files.`,
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer done()
		if err := godotenv.Load(); err != nil {
			logger.Log.Error("error loading config", slog.Any("err", err))
			os.Exit(1)
		}

		direction, _ := cmd.Flags().GetString("direction")
		numOfMigrations, _ := cmd.Flags().GetInt("num")

		deps, err := newMigrateDeps(ctx)

		if err != nil {
			logger.Log.Error("error while initializing dependencies", slog.Any("err", err))
			os.Exit(1)
		}

		defer func() {
			done()
			deps.Close(ctx)
		}()

		switch direction {
		case "up":
			if err := runMigrationsUp(deps.Migration, numOfMigrations); err != nil {
				logger.Log.Error("Error while applying migrations", slog.Any("err", err))
				os.Exit(1)
			}
		case "down":
			if err := runMigrationsDown(deps.Migration, numOfMigrations); err != nil {
				logger.Log.Error("Error while rolling back migrations", slog.Any("err", err))
				os.Exit(1)
			}
		}
	},
}

func runMigrationsUp(migration *migrate.Migrate, num int) error {
	if num == 0 {
		if err := migration.Up(); err != nil && err != migrate.ErrNoChange {
			return err
		}
		logger.Log.Info("applied all migrations")
		return nil
	}

	if err := migration.Steps(num); err != nil && err != migrate.ErrNoChange {
		return err
	}

	logger.Log.Info("applied migrations", slog.Int("appliedMigrations", num))
	return nil
}

func runMigrationsDown(migration *migrate.Migrate, num int) error {
	if num == 0 {
		if err := migration.Down(); err != nil && err != migrate.ErrNoChange {
			return err
		}
		logger.Log.Info("rolled back all migrations")
		return nil
	}

	if err := migration.Steps(-num); err != nil && err != migrate.ErrNoChange {
		return err
	}

	logger.Log.Info("rolled back migrations", slog.Int("rolledBackMigrations", num))
	return nil
}

func init() {
	MigrateCmd.Flags().StringP("direction", "d", "up", "can be any of 'up' or 'down' (default: up)")
	MigrateCmd.Flags().IntP("num", "n", 0, "number of migrations to apply")
}
