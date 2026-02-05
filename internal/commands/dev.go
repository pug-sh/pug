package commands

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/fivebitsio/cotton/internal/deps/logger"
	"github.com/joho/godotenv"
	"github.com/spf13/cobra"
)

var DevCmd = &cobra.Command{
	Use:   "dev",
	Short: "Start the Cotton server and workers for development",
	Long:  `Start the Cotton server and all workers together for local development.`,
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer done()

		if err := godotenv.Load(); err != nil {
			logger.Log.Warn("No .env file found, relying on environment variables", slog.Any("err", err))
		}

		serverDeps, err := newServerDeps(ctx)
		if err != nil {
			logger.Log.Error("error while initializing server dependencies", slog.Any("err", err))
			os.Exit(1)
		}
		defer serverDeps.Close(ctx)

		workerDeps, err := newWorkerDeps(ctx)
		if err != nil {
			logger.Log.Error("error while initializing worker dependencies", slog.Any("err", err))
			os.Exit(1)
		}
		defer workerDeps.Close(ctx)

		workerErrChan := make(chan error, 2)
		go func() {
			workerErrChan <- StartSubscriptionWorker(ctx, workerDeps)
		}()
		go func() {
			workerErrChan <- StartCampaignWorker(ctx, workerDeps)
		}()

		serverErrChan := make(chan error, 1)
		go func() {
			serverErrChan <- StartServer(ctx, serverDeps)
		}()

		select {
		case serverErr := <-serverErrChan:
			logger.Log.Info("Server stopped", slog.Any("err", serverErr))
		case workerErr := <-workerErrChan:
			logger.Log.Info("Worker stopped", slog.Any("err", workerErr))
		case <-ctx.Done():
			logger.Log.Info("Shutdown signal received")
		}

		logger.Log.Info("Shutting down...")
	},
}
