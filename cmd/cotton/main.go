package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/fivebitsio/cotton/internal/app/migrate/clickhouse"
	migratenats "github.com/fivebitsio/cotton/internal/app/migrate/nats"
	"github.com/fivebitsio/cotton/internal/app/migrate/postgres"
	"github.com/fivebitsio/cotton/internal/app/server"
	"github.com/fivebitsio/cotton/internal/app/workers/campaigns"
	"github.com/fivebitsio/cotton/internal/app/workers/subscriptions"
	"github.com/joho/godotenv"
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "cotton",
	Short: "Cotton is a unified command line tool for managing the Cotton application",
}

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Start the Cotton server",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer done()

		if err := godotenv.Load(); err != nil {
			slog.Debug("No .env file found, relying on environment variables")
		}

		if err := server.Run(ctx); err != nil {
			slog.Error("server error", slog.Any("err", err))
			os.Exit(1)
		}
	},
}

var workerCmd = &cobra.Command{
	Use:   "worker",
	Short: "Worker related commands",
}

var subscriptionCmd = &cobra.Command{
	Use:   "subscription",
	Short: "Start the subscription worker",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer done()

		if err := godotenv.Load(); err != nil {
			slog.Debug("No .env file found, relying on environment variables")
		}

		if err := subscriptions.Run(ctx); err != nil {
			slog.Error("error starting subscription worker", slog.Any("err", err))
			os.Exit(1)
		}
	},
}

var campaignCmd = &cobra.Command{
	Use:   "campaign",
	Short: "Start the campaign worker",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer done()

		if err := godotenv.Load(); err != nil {
			slog.Debug("No .env file found, relying on environment variables")
		}

		if err := campaigns.Run(ctx); err != nil {
			slog.Error("error starting campaign worker", slog.Any("err", err))
			os.Exit(1)
		}
	},
}

var devCmd = &cobra.Command{
	Use:   "dev",
	Short: "Start the Cotton server and workers for development",
	Run: func(cmd *cobra.Command, args []string) {
		sigCtx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer done()

		ctx, cancel := context.WithCancel(sigCtx)
		defer cancel()

		if err := godotenv.Load(); err != nil {
			slog.Debug("No .env file found, relying on environment variables")
		}

		errChan := make(chan error, 3)
		go func() { errChan <- subscriptions.Run(ctx) }()
		go func() { errChan <- campaigns.Run(ctx) }()
		go func() { errChan <- server.Run(ctx) }()

		select {
		case err := <-errChan:
			slog.Error("component stopped", slog.Any("err", err))
			cancel()
		case <-ctx.Done():
			slog.Info("Shutdown signal received")
		}

		for i := 0; i < 2; i++ {
			if err := <-errChan; err != nil && ctx.Err() == nil {
				slog.Error("component stopped during shutdown", slog.Any("err", err))
			}
		}

		slog.Info("Shutting down...")
	},
}

var postgresMigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Run database migrations for postgres",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer done()

		if err := godotenv.Load(); err != nil {
			slog.Debug("No .env file found, relying on environment variables")
		}

		direction, _ := cmd.Flags().GetString("direction")
		num, _ := cmd.Flags().GetInt("num")

		var err error
		switch direction {
		case "up":
			err = postgres.Up(ctx, num)
		case "down":
			err = postgres.Down(ctx, num)
		default:
			slog.Error("invalid migration direction, must be 'up' or 'down'", slog.String("direction", direction))
			os.Exit(1)
		}
		if err != nil {
			slog.Error("postgres migration error", slog.Any("err", err))
			os.Exit(1)
		}
	},
}

var natsMigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Initialize NATS streams and consumers",
	Run: func(cmd *cobra.Command, args []string) {
		ctx := context.Background()

		if err := godotenv.Load(); err != nil {
			slog.Debug("No .env file found")
		}

		if err := migratenats.Run(ctx); err != nil {
			slog.Error("NATS initialization error", slog.Any("err", err))
			os.Exit(1)
		}
	},
}

var clickhouseMigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Run database migrations for clickhouse",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer done()

		if err := godotenv.Load(); err != nil {
			slog.Debug("No .env file found, relying on environment variables")
		}

		direction, _ := cmd.Flags().GetString("direction")
		num, _ := cmd.Flags().GetInt("num")

		var err error
		switch direction {
		case "up":
			err = clickhouse.Up(ctx, num)
		case "down":
			err = clickhouse.Down(ctx, num)
		default:
			slog.Error("invalid migration direction, must be 'up' or 'down'", slog.String("direction", direction))
			os.Exit(1)
		}
		if err != nil {
			slog.Error("clickhouse migration error", slog.Any("err", err))
			os.Exit(1)
		}
	},
}

func init() {
	workerCmd.AddCommand(subscriptionCmd)
	workerCmd.AddCommand(campaignCmd)

	rootCmd.AddCommand(serverCmd)
	rootCmd.AddCommand(workerCmd)
	rootCmd.AddCommand(devCmd)

	postgresMigrateCmd.Flags().StringP("direction", "d", "up", "can be any of 'up' or 'down' (default: up)")
	postgresMigrateCmd.Flags().IntP("num", "n", 0, "number of migrations to apply")

	postgresCmd := &cobra.Command{
		Use:   "postgres",
		Short: "PostgreSQL related commands",
	}
	postgresCmd.AddCommand(postgresMigrateCmd)

	natsCmd := &cobra.Command{
		Use:   "nats",
		Short: "NATS related commands",
	}
	natsCmd.AddCommand(natsMigrateCmd)

	clickhouseMigrateCmd.Flags().StringP("direction", "d", "up", "can be any of 'up' or 'down' (default: up)")
	clickhouseMigrateCmd.Flags().IntP("num", "n", 0, "number of migrations to apply")

	clickhouseCmd := &cobra.Command{
		Use:   "clickhouse",
		Short: "ClickHouse related commands",
	}
	clickhouseCmd.AddCommand(clickhouseMigrateCmd)

	rootCmd.AddCommand(postgresCmd)
	rootCmd.AddCommand(natsCmd)
	rootCmd.AddCommand(clickhouseCmd)
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := rootCmd.ExecuteContext(context.Background()); err != nil {
		os.Exit(1)
	}
}
