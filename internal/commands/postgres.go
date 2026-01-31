package commands

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/fivebitsio/cotton/pkg/logger"
	"github.com/fivebitsio/cotton/pkg/postgres"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/joho/godotenv"
	"github.com/pressly/goose/v3"
	"github.com/sethvargo/go-envconfig"
	"github.com/spf13/cobra"
)

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

		var databaseConfig postgres.Config
		if err := envconfig.Process(ctx, &databaseConfig); err != nil {
			logger.Log.Error("error loading database config", slog.Any("err", err))
			os.Exit(1)
		}

		db, err := sql.Open("pgx", databaseConfig.ConnectionString())
		if err != nil {
			logger.Log.Error("error opening database connection", slog.Any("err", err))
			os.Exit(1)
		}
		defer db.Close()

		wd, err := os.Getwd()
		if err != nil {
			logger.Log.Error("error getting working directory", slog.Any("err", err))
			os.Exit(1)
		}
		migrationsDir := filepath.Join(wd, "schema", "postgres", "migrations")

		if err := goose.SetDialect("postgres"); err != nil {
			logger.Log.Error("error setting goose dialect", slog.Any("err", err))
			os.Exit(1)
		}

		switch direction {
		case "up":
			if err := runMigrationsUp(ctx, db, migrationsDir, numOfMigrations); err != nil {
				logger.Log.Error("Error while applying migrations", slog.Any("err", err))
				os.Exit(1)
			}
		case "down":
			if err := runMigrationsDown(ctx, db, migrationsDir, numOfMigrations); err != nil {
				logger.Log.Error("Error while rolling back migrations", slog.Any("err", err))
				os.Exit(1)
			}
		}
	},
}

func runMigrationsUp(ctx context.Context, db *sql.DB, dir string, num int) error {
	if num == 0 {
		if err := goose.UpContext(ctx, db, dir); err != nil {
			return err
		}
		logger.Log.Info("applied all migrations")
		return nil
	}

	current, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		return err
	}
	target := current + int64(num)

	if err := goose.UpToContext(ctx, db, dir, target); err != nil {
		return err
	}

	logger.Log.Info("applied migrations", slog.Int("appliedMigrations", num))
	return nil
}

func runMigrationsDown(ctx context.Context, db *sql.DB, dir string, num int) error {
	if num == 0 {
		for {
			current, err := goose.GetDBVersionContext(ctx, db)
			if err != nil {
				return err
			}
			if current == 0 {
				break
			}
			if err := goose.DownContext(ctx, db, dir); err != nil {
				return err
			}
		}
		logger.Log.Info("rolled back all migrations")
		return nil
	}

	for i := 0; i < num; i++ {
		if err := goose.DownContext(ctx, db, dir); err != nil {
			return err
		}
	}

	logger.Log.Info("rolled back migrations", slog.Int("rolledBackMigrations", num))
	return nil
}

func init() {
	MigrateCmd.Flags().StringP("direction", "d", "up", "can be any of 'up' or 'down' (default: up)")
	MigrateCmd.Flags().IntP("num", "n", 0, "number of migrations to apply")
}
