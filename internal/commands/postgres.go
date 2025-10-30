package commands

import (
	"context"
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
	"github.com/spf13/cobra"
)

type migrateDeps struct {
	DB *postgres.DB
}

func (deps *migrateDeps) Close(ctx context.Context) error {
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

	return &migrateDeps{
		DB: db,
	}, nil
}

// MigrateCmd represents the postgres migrate command
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

		migrations := &migrate.FileMigrationSource{
			Dir: "schema/postgres/migrations",
		}

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
			appliedMigrations, err := migrate.ExecMax(
				stdlib.OpenDBFromPool(deps.DB.Pool),
				"postgres",
				migrations,
				migrate.Up,
				numOfMigrations,
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
				numOfMigrations,
			)

			if err != nil {
				logger.Log.Error(
					"Error while applying migrations",
					slog.Any("err", err))
			}

			logger.Log.Info("applied migrations", slog.Int("appliedMigrations", appliedMigrations))
		}
	},
}

func init() {
	MigrateCmd.Flags().StringP("direction", "d", "up", "can be any of 'up' or 'down'")
	MigrateCmd.Flags().IntP("num", "n", 0, "number of migrations to apply")
}
