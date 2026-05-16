package email

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/pug-sh/pug/internal/core/email"
	resenddeps "github.com/pug-sh/pug/internal/deps/email/resend"
	natsworker "github.com/pug-sh/pug/internal/deps/nats"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/sethvargo/go-envconfig"
)

func Run(ctx context.Context) error {
	closeOtel, err := telemetry.SetupSDK(ctx)
	if err != nil {
		return err
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := closeOtel(shutdownCtx); err != nil {
			slog.ErrorContext(shutdownCtx, "failed to shutdown telemetry", slogx.Error(err))
		}
	}()

	var pgCfg postgres.Config
	if err := envconfig.Process(ctx, &pgCfg); err != nil {
		return err
	}
	pgRO, err := postgres.NewReaderPool(ctx, &pgCfg)
	if err != nil {
		return err
	}
	defer pgRO.Close()

	natsClient, err := natsworker.New(ctx)
	if err != nil {
		return err
	}
	defer natsClient.Close()

	var emailCfg email.Config
	if err := envconfig.Process(ctx, &emailCfg); err != nil {
		return err
	}
	var resendCfg resenddeps.Config
	if err := envconfig.Process(ctx, &resendCfg); err != nil {
		return err
	}
	provider, err := resenddeps.New(resendCfg)
	if err != nil {
		return err
	}
	mailer, err := email.NewService(emailCfg, provider)
	if err != nil {
		return err
	}

	return StartWorker(ctx, pgRO, natsClient, mailer)
}

func StartWorker(ctx context.Context, pgRO *pgxpool.Pool, natsClient *natsworker.NATSClient, mailer *email.Service) error {
	consumerConfig, err := natsClient.GetConsumerConfigByName("misc-email-processor")
	if err != nil {
		return fmt.Errorf("failed to get misc email consumer config: %w", err)
	}

	processor := NewProcessor(dbread.New(pgRO), mailer)
	messageProcessor := func(ctx context.Context, msg jetstream.Msg) error {
		return processor.ProcessMessage(ctx, msg.Data())
	}

	worker, err := natsworker.NewWorker(natsworker.WorkerConfig{
		StreamName:        consumerConfig.StreamName,
		ConsumerName:      consumerConfig.DurableName,
		DurableName:       consumerConfig.DurableName,
		FilterSubject:     consumerConfig.FilterSubject,
		Concurrency:       4,
		ProcessingTimeout: 30 * time.Second,
		MaxDeliver:        consumerConfig.MaxDeliver,
		AckWait:           30 * time.Second,
		DLQSubject:        natsworker.DLQMiscEmailSubject,
	}, messageProcessor, natsClient)
	if err != nil {
		return err
	}

	return worker.Start(ctx)
}
