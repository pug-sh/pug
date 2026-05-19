package email

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	coreemail "github.com/pug-sh/pug/internal/core/email"
	natsworker "github.com/pug-sh/pug/internal/deps/nats"
	"github.com/pug-sh/pug/internal/deps/postgres"
	pugredis "github.com/pug-sh/pug/internal/deps/redis"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/slogx"
	goredis "github.com/redis/go-redis/v9"
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

	// Redis is only required when PUG_EMAIL_PROVIDER_SECRET_KEY is set
	// (per-tenant cache). In operator-only mode we skip the connection so
	// boot doesn't depend on Redis.
	var keyCfg secretKeyConfig
	if err := envconfig.Process(ctx, &keyCfg); err != nil {
		return err
	}
	var rdClient *pugredis.Client
	if needsTenantCache(keyCfg.KeyB64) {
		var rdCfg pugredis.Config
		if err := envconfig.Process(ctx, &rdCfg); err != nil {
			return err
		}
		rdClient, err = pugredis.NewFromConfig(ctx, &rdCfg)
		if err != nil {
			return err
		}
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			rdClient.Close(shutdownCtx)
		}()
	}

	var cache *goredis.Client
	if rdClient != nil {
		cache = rdClient.Unwrap()
	}
	mailer, err := newMailerWithResolver(ctx, dbread.New(pgRO), cache)
	if err != nil {
		return err
	}

	return StartWorker(ctx, pgRO, natsClient, mailer)
}

func StartWorker(ctx context.Context, pgRO *pgxpool.Pool, natsClient *natsworker.NATSClient, mailer *coreemail.Service) error {
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

// needsTenantCache reports whether the worker should connect to Redis. When
// PUG_EMAIL_PROVIDER_SECRET_KEY is unset the worker runs in operator-only
// mode and the cache is not used, so the Redis connection is skipped to
// keep boot from depending on a service we don't read.
func needsTenantCache(secretKeyB64 string) bool {
	return secretKeyB64 != ""
}
