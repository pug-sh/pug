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
	chseed "github.com/fivebitsio/cotton/internal/app/seed/clickhouse"
	pgseed "github.com/fivebitsio/cotton/internal/app/seed/postgres"
	"github.com/fivebitsio/cotton/internal/app/server"
	"github.com/fivebitsio/cotton/internal/app/workers/campaigns"
	"github.com/fivebitsio/cotton/internal/app/workers/devices"
	eventsworker "github.com/fivebitsio/cotton/internal/app/workers/events"
	"github.com/fivebitsio/cotton/internal/app/workers/profiles/alias"
	"github.com/fivebitsio/cotton/internal/app/workers/profiles/identify"
	"github.com/fivebitsio/cotton/internal/app/workers/profiles/register"
	"github.com/fivebitsio/cotton/internal/app/workers/scheduler"
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

var profileCmd = &cobra.Command{
	Use:   "profile",
	Short: "Profile worker related commands",
}

var profileRegisterCmd = &cobra.Command{
	Use:   "register",
	Short: "Start the profile register worker",
	Run:   run(register.Run),
}

var profileIdentifyCmd = &cobra.Command{
	Use:   "identify",
	Short: "Start the profile identify worker",
	Run:   run(identify.Run),
}

var profileAliasCmd = &cobra.Command{
	Use:   "alias",
	Short: "Start the profile alias worker",
	Run:   run(alias.Run),
}

var deviceCmd = &cobra.Command{
	Use:   "device",
	Short: "Start the device worker",
	Run:   run(devices.Run),
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

var schedulerCmd = &cobra.Command{
	Use:   "scheduler",
	Short: "Start the scheduler worker",
	Run:   run(scheduler.Run),
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
		g.Go(func() error { return devices.Run(ctx) })
		g.Go(func() error { return campaigns.Run(ctx) })
		g.Go(func() error { return eventsworker.Run(ctx) })
		g.Go(func() error { return register.Run(ctx) })
		g.Go(func() error { return identify.Run(ctx) })
		g.Go(func() error { return alias.Run(ctx) })
		g.Go(func() error { return scheduler.Run(ctx) })
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

var clickhouseSeedCmd = &cobra.Command{
	Use:   "seed",
	Short: "Seed ClickHouse with mock events for the first user and project",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer done()

		if err := godotenv.Load(); err != nil {
			slog.DebugContext(ctx, "No .env file found, relying on environment variables")
		}

		count, _ := cmd.Flags().GetInt64("count")
		batchSize, _ := cmd.Flags().GetInt("batch")

		if err := chseed.Run(ctx, count, batchSize); err != nil {
			slog.ErrorContext(ctx, "seed error", slog.Any("err", err))
			os.Exit(1)
		}
	},
}

var postgresSeedCmd = &cobra.Command{
	Use:   "seed",
	Short: "Seed PostgreSQL with test user and default project",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer done()

		if err := godotenv.Load(); err != nil {
			slog.DebugContext(ctx, "No .env file found, relying on environment variables")
		}

		if err := pgseed.Run(ctx); err != nil {
			slog.ErrorContext(ctx, "seed error", slog.Any("err", err))
			os.Exit(1)
		}
	},
}

func init() {
	profileCmd.AddCommand(profileRegisterCmd)
	profileCmd.AddCommand(profileIdentifyCmd)
	profileCmd.AddCommand(profileAliasCmd)
	workerCmd.AddCommand(profileCmd)
	workerCmd.AddCommand(deviceCmd)
	workerCmd.AddCommand(campaignCmd)
	workerCmd.AddCommand(eventsCmd)
	workerCmd.AddCommand(schedulerCmd)

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
	postgresCmd.AddCommand(postgresSeedCmd)

	natsCmd := &cobra.Command{
		Use:   "nats",
		Short: "NATS related commands",
	}
	natsCmd.AddCommand(natsMigrateCmd)

	clickhouseMigrateCmd.Flags().StringP("direction", "d", "up", "can be any of 'up' or 'down' (default: up)")
	clickhouseMigrateCmd.Flags().IntP("num", "n", 0, "number of migrations to apply")

	clickhouseSeedCmd.Flags().Int64P("count", "c", 10_000_000, "total number of events to generate")
	clickhouseSeedCmd.Flags().IntP("batch", "b", 10_000, "number of events per ClickHouse batch")

	clickhouseCmd := &cobra.Command{
		Use:   "clickhouse",
		Short: "ClickHouse related commands",
	}
	clickhouseCmd.AddCommand(clickhouseMigrateCmd)
	clickhouseCmd.AddCommand(clickhouseSeedCmd)

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
