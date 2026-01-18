package commands

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/fivebitsio/cotton/internal/workers/campaigns"
	"github.com/fivebitsio/cotton/internal/workers/subscriptions"
	"github.com/fivebitsio/cotton/pkg/logger"
	"github.com/fivebitsio/cotton/pkg/nats"
	"github.com/fivebitsio/cotton/pkg/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/sethvargo/go-envconfig"
	"github.com/spf13/cobra"
)

type workerDeps struct {
	pgRo  *pgxpool.Pool
	pgW   *pgxpool.Pool
	nats  *nats.NATSClient
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

	natsClient, err := nats.New(ctx)
	if err != nil {
		return nil, err
	}

	return &workerDeps{
		pgRo:  pgRo,
		pgW:   pgW,
		nats:  natsClient,
	}, nil
}

func StartCampaignWorker(ctx context.Context, deps *workerDeps) error {
	logger.Log.Info("Starting campaign worker...")

	return campaigns.StartWorker(ctx, deps.pgRo, deps.pgW, deps.nats)
}

func StartSubscriptionWorker(ctx context.Context, deps *workerDeps) error {
	logger.Log.Info("Starting subscription worker...")

	return subscriptions.StartWorker(ctx, deps.pgRo, deps.pgW, deps.nats)
}

func (deps *workerDeps) Close(ctx context.Context) {
	deps.pgRo.Close()
	deps.pgW.Close()
	if deps.nats != nil {
		deps.nats.Close()
	}
}

var WorkerCmd = &cobra.Command{
	Use:   "worker",
	Short: "Worker related commands",
	Long:  `Commands for managing message workers.`,
}

var SubscriptionWorkerCmd = &cobra.Command{
	Use:   "subscription",
	Short: "Start the subscription worker",
	Long:  `Start the subscription worker that processes subscription operation messages from NATS.`,
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

		if err := StartSubscriptionWorker(ctx, deps); err != nil {
			logger.Log.Error("error starting subscription worker", slog.Any("err", err))
			os.Exit(1)
		}
	},
}

var CampaignWorkerCmd = &cobra.Command{
	Use:   "campaign",
	Short: "Start the campaign worker",
	Long:  `Start the campaign worker that processes campaign scheduling messages from NATS.`,
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

		if err := StartCampaignWorker(ctx, deps); err != nil {
			logger.Log.Error("error starting campaign worker", slog.Any("err", err))
			os.Exit(1)
		}
	},
}

func init() {
	WorkerCmd.AddCommand(SubscriptionWorkerCmd)
	WorkerCmd.AddCommand(CampaignWorkerCmd)
}
