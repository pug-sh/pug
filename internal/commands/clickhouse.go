package commands

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	_ "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/fivebitsio/cotton/pkg/clickhouse"
	"github.com/fivebitsio/cotton/pkg/logger"
	"github.com/joho/godotenv"
	"github.com/pressly/goose/v3"
	"github.com/sethvargo/go-envconfig"
	"github.com/spf13/cobra"
)

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

		var databaseConfig clickhouse.Config
		if err := envconfig.Process(ctx, &databaseConfig); err != nil {
			logger.Log.Error("error loading clickhouse config", slog.Any("err", err))
			os.Exit(1)
		}

		db, err := sql.Open("clickhouse", databaseConfig.DSN())
		if err != nil {
			logger.Log.Error("error opening clickhouse connection", slog.Any("err", err))
			os.Exit(1)
		}
		defer db.Close()

		wd, err := os.Getwd()
		if err != nil {
			logger.Log.Error("error getting working directory", slog.Any("err", err))
			os.Exit(1)
		}
		migrationsDir := filepath.Join(wd, "schema", "clickhouse", "migrations")

		if err := goose.SetDialect("clickhouse"); err != nil {
			logger.Log.Error("error setting goose dialect", slog.Any("err", err))
			os.Exit(1)
		}

		switch direction {
		case "up":
			if err := runClickhouseMigrationsUp(ctx, db, migrationsDir, numOfMigrations); err != nil {
				logger.Log.Error("Error while applying clickhouse migrations", slog.Any("err", err))
				os.Exit(1)
			}
		case "down":
			if err := runClickhouseMigrationsDown(ctx, db, migrationsDir, numOfMigrations); err != nil {
				logger.Log.Error("Error while rolling back clickhouse migrations", slog.Any("err", err))
				os.Exit(1)
			}
		}
	},
}

func runClickhouseMigrationsUp(ctx context.Context, db *sql.DB, dir string, num int) error {
	if num == 0 {
		if err := goose.UpContext(ctx, db, dir); err != nil {
			return err
		}
		logger.Log.Info("applied all clickhouse migrations")
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

	logger.Log.Info("applied clickhouse migrations", slog.Int("appliedMigrations", num))
	return nil
}

func runClickhouseMigrationsDown(ctx context.Context, db *sql.DB, dir string, num int) error {
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
		logger.Log.Info("rolled back all clickhouse migrations")
		return nil
	}

	for i := 0; i < num; i++ {
		if err := goose.DownContext(ctx, db, dir); err != nil {
			return err
		}
	}

	logger.Log.Info("rolled back clickhouse migrations", slog.Int("rolledBackMigrations", num))
	return nil
}

func init() {
	ClickhouseMigrateCmd.Flags().StringP("direction", "d", "up", "can be any of 'up' or 'down' (default: up)")
	ClickhouseMigrateCmd.Flags().IntP("num", "n", 0, "number of migrations to apply")
}
