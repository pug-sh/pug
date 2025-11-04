package commands

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/fivebitsio/cotton/internal/workers/subscriptions"
	"github.com/fivebitsio/cotton/pkg/logger"
	"github.com/fivebitsio/cotton/pkg/postgres"
	"github.com/fivebitsio/cotton/pkg/pulsar"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/sethvargo/go-envconfig"
	"github.com/spf13/cobra"
)

type workerDeps struct {
	pgRo   *pgxpool.Pool
	pgW    *pgxpool.Pool
	pulsar *pulsar.Client
}

func newWorkerDeps(ctx context.Context) (*workerDeps, error) {
	var cfg postgres.Config
	if err := envconfig.Process(ctx, &cfg); err != nil {
		return nil, err
	}

	pgRo, err := postgres.NewReaderPool(ctx, &cfg)
	if err != nil {
		return nil, err
	}

	pgW, err := postgres.NewWriterPool(ctx, &cfg)
	if err != nil {
		return nil, err
	}

	pulsarClient, err := pulsar.NewClient(ctx)
	if err != nil {
		return nil, err
	}

	return &workerDeps{
		pgRo:   pgRo,
		pgW:    pgW,
		pulsar: pulsarClient,
	}, nil
}

func (deps *workerDeps) Close(ctx context.Context) {
	deps.pgRo.Close()
	deps.pgW.Close()
	if deps.pulsar != nil {
		deps.pulsar.Close()
	}
}

// WorkerCmd represents the worker command
var WorkerCmd = &cobra.Command{
	Use:   "worker",
	Short: "Worker related commands",
	Long:  `Commands for managing message workers.`,
}

// SubscriptionWorkerCmd represents the subscription worker command
var SubscriptionWorkerCmd = &cobra.Command{
	Use:   "subscription",
	Short: "Start the subscription worker",
	Long:  `Start the subscription worker that processes subscription operation messages from Pulsar.`,
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer done()

		if err := godotenv.Load(); err != nil {
			logger.Log.Error("error loading .env file", slog.Any("err", err))
			os.Exit(1)
		}

		deps, err := newWorkerDeps(ctx)
		if err != nil {
			logger.Log.Error("error while initializing worker dependencies", slog.Any("err", err))
			os.Exit(1)
		}
		defer deps.Close(ctx)

		logger.Log.Info("Starting subscription worker...")

		if err := subscriptions.StartWorker(ctx, deps.pgRo, deps.pgW, deps.pulsar); err != nil {
			logger.Log.Error("error starting subscription worker", slog.Any("err", err))
			os.Exit(1)
		}
	},
}

func init() {
	WorkerCmd.AddCommand(SubscriptionWorkerCmd)
}
