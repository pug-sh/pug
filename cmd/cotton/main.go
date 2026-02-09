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
	eventsworker "github.com/fivebitsio/cotton/internal/app/workers/events"
	"github.com/fivebitsio/cotton/internal/app/workers/subscriptions"
	"github.com/joho/godotenv"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

// run creates a signal-aware context, loads .env, and runs fn.
func run(fn func(ctx context.Context) error) func(cmd *cobra.Command, args []string) {
	return func(cmd *cobra.Command, args []string) {
		ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer done()

		if err := godotenv.Load(); err != nil {
			slog.DebugContext(ctx, "No .env file found, relying on environment variables")
		}

		if err := fn(ctx); err != nil {
			slog.ErrorContext(ctx, "fatal error", slog.Any("err", err))
			os.Exit(1)
		}
	}
}

// runMigrate creates a signal-aware context, loads .env, reads --direction and --num flags,
// validates direction, and calls the appropriate up/down function.
func runMigrate(up, down func(ctx context.Context, num int) error) func(cmd *cobra.Command, args []string) {
	return func(cmd *cobra.Command, args []string) {
		ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer done()

		if err := godotenv.Load(); err != nil {
			slog.DebugContext(ctx, "No .env file found, relying on environment variables")
		}

		direction, _ := cmd.Flags().GetString("direction")
		num, _ := cmd.Flags().GetInt("num")

		var err error
		switch direction {
		case "up":
			err = up(ctx, num)
		case "down":
			err = down(ctx, num)
		default:
			slog.ErrorContext(ctx, "invalid migration direction, must be 'up' or 'down'", slog.String("direction", direction))
			os.Exit(1)
		}
		if err != nil {
			slog.ErrorContext(ctx, "migration error", slog.Any("err", err))
			os.Exit(1)
		}
	}
}

var rootCmd = &cobra.Command{
	Use:   "cotton",
	Short: "Cotton is a unified command line tool for managing the Cotton application",
}

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Start the Cotton server",
	Run:   run(server.Run),
}

var workerCmd = &cobra.Command{
	Use:   "worker",
	Short: "Worker related commands",
}

var subscriptionCmd = &cobra.Command{
	Use:   "subscription",
	Short: "Start the subscription worker",
	Run:   run(subscriptions.Run),
}

var campaignCmd = &cobra.Command{
	Use:   "campaign",
	Short: "Start the campaign worker",
	Run:   run(campaigns.Run),
}

var eventsCmd = &cobra.Command{
	Use:   "events",
	Short: "Start the events worker",
	Run:   run(eventsworker.Run),
}

var devCmd = &cobra.Command{
	Use:   "dev",
	Short: "Start the Cotton server and workers for development",
	Run: func(cmd *cobra.Command, args []string) {
		sigCtx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer done()

		if err := godotenv.Load(); err != nil {
			slog.DebugContext(sigCtx, "No .env file found, relying on environment variables")
		}

		g, ctx := errgroup.WithContext(sigCtx)
		g.Go(func() error { return subscriptions.Run(ctx) })
		g.Go(func() error { return campaigns.Run(ctx) })
		g.Go(func() error { return eventsworker.Run(ctx) })
		g.Go(func() error { return server.Run(ctx) })

		if err := g.Wait(); err != nil {
			slog.ErrorContext(sigCtx, "component stopped", slog.Any("err", err))
		}

		slog.InfoContext(sigCtx, "Shutting down...")
	},
}

var postgresMigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Run database migrations for postgres",
	Run:   runMigrate(postgres.Up, postgres.Down),
}

var natsMigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Initialize NATS streams and consumers",
	Run:   run(migratenats.Run),
}

var clickhouseMigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Run database migrations for clickhouse",
	Run:   runMigrate(clickhouse.Up, clickhouse.Down),
}

func init() {
	workerCmd.AddCommand(subscriptionCmd)
	workerCmd.AddCommand(campaignCmd)
	workerCmd.AddCommand(eventsCmd)

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
