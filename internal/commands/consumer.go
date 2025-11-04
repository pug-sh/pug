package commands

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/fivebitsio/cotton/internal/consumers/subscriptions"
	"github.com/fivebitsio/cotton/pkg/logger"
	"github.com/fivebitsio/cotton/pkg/postgres"
	"github.com/fivebitsio/cotton/pkg/pulsar"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/sethvargo/go-envconfig"
	"github.com/spf13/cobra"
)

type consumerDeps struct {
	pgRo   *pgxpool.Pool
	pgW    *pgxpool.Pool
	pulsar *pulsar.Client
}

func newConsumerDeps(ctx context.Context) (*consumerDeps, error) {
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

	return &consumerDeps{
		pgRo:   pgRo,
		pgW:    pgW,
		pulsar: pulsarClient,
	}, nil
}

func (deps *consumerDeps) Close(ctx context.Context) {
	deps.pgRo.Close()
	deps.pgW.Close()
	if deps.pulsar != nil {
		deps.pulsar.Close()
	}
}

// ConsumerCmd represents the consumer command
var ConsumerCmd = &cobra.Command{
	Use:   "consumer",
	Short: "Consumer related commands",
	Long:  `Commands for managing message consumers.`,
}

// SubscriptionConsumerCmd represents the subscription consumer command
var SubscriptionConsumerCmd = &cobra.Command{
	Use:   "subscription",
	Short: "Start the subscription consumer",
	Long:  `Start the subscription consumer that processes subscription operation messages from Pulsar.`,
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer done()

		if err := godotenv.Load(); err != nil {
			logger.Log.Error("error loading .env file", slog.Any("err", err))
			os.Exit(1)
		}

		deps, err := newConsumerDeps(ctx)
		if err != nil {
			logger.Log.Error("error while initializing consumer dependencies", slog.Any("err", err))
			os.Exit(1)
		}
		defer deps.Close(ctx)

		logger.Log.Info("Starting subscription consumer...")

		if err := subscriptions.StartConsumer(ctx, deps.pgRo, deps.pgW, deps.pulsar); err != nil {
			logger.Log.Error("error starting subscription consumer", slog.Any("err", err))
			os.Exit(1)
		}
	},
}

func init() {
	ConsumerCmd.AddCommand(SubscriptionConsumerCmd)
}
