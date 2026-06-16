package compliance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"buf.build/go/protovalidate"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	coreprofiles "github.com/pug-sh/pug/internal/core/profiles"
	"github.com/pug-sh/pug/internal/deps/clickhouse"
	natsworker "github.com/pug-sh/pug/internal/deps/nats"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	workercompliancev1 "github.com/pug-sh/pug/internal/gen/proto/workers/compliance/v1"
	"github.com/pug-sh/pug/internal/slogx"
	"github.com/sethvargo/go-envconfig"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"
)

// The compliance worker hosts the slow, low-volume GDPR/DPDP jobs that share a
// long-timeout consumer profile, distinct from the millisecond hot-path workers.
// Erasure (§4.1) is the first tenant; data-subject export (§4.2) and
// retention/TTL purge (§4.5) add sibling consumers in StartWorker. The heavy
// ClickHouse mutations run here, never inline in an RPC. See
// docs/compliance/4.1-erasure-scope.md.

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
	pgW, err := postgres.NewWriterPool(ctx, &pgCfg)
	if err != nil {
		return err
	}
	defer pgW.Close()

	var chCfg clickhouse.Config
	if err := envconfig.Process(ctx, &chCfg); err != nil {
		return err
	}
	chDB, err := clickhouse.NewFromConfig(ctx, &chCfg)
	if err != nil {
		return err
	}
	defer func() {
		if err := chDB.Close(ctx); err != nil {
			slog.WarnContext(ctx, "failed to close ClickHouse connection", slogx.Error(err))
		}
	}()

	natsClient, err := natsworker.New(ctx)
	if err != nil {
		return err
	}
	defer natsClient.Close()

	slog.InfoContext(ctx, "Starting compliance worker...")
	return StartWorker(ctx, pgW, chDB.Conn, natsClient)
}

func StartWorker(ctx context.Context, pgW *pgxpool.Pool, ch driver.Conn, natsClient *natsworker.NATSClient) error {
	svc := coreprofiles.NewService(pgW, ch, natsClient)

	// One process, one consumer per compliance job. Export (§4.2) and retention
	// (§4.5) slot in as additional g.Go(...) consumers here.
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return runEraseConsumer(ctx, svc, natsClient) })
	return g.Wait()
}

func runEraseConsumer(ctx context.Context, svc *coreprofiles.Service, natsClient *natsworker.NATSClient) error {
	consumerConfig, err := natsClient.GetConsumerConfigByName("compliance-erase-processor-durable")
	if err != nil {
		return fmt.Errorf("failed to get compliance erase consumer config: %w", err)
	}

	config := natsworker.WorkerConfig{
		StreamName:    consumerConfig.StreamName,
		ConsumerName:  consumerConfig.DurableName,
		DurableName:   consumerConfig.DurableName,
		FilterSubject: consumerConfig.FilterSubject,
		// Erasures are infrequent and idempotent; keep concurrency low so a burst
		// doesn't flood ClickHouse with parallel mutations.
		Concurrency: 2,
		// Heavy ClickHouse mutations run synchronously (mutations_sync=1), so a
		// single message can take a while. Allow generous time; an over-long
		// mutation just redelivers and re-runs idempotently (frozen identifiers).
		ProcessingTimeout: 5 * time.Minute,
		MaxDeliver:        consumerConfig.MaxDeliver,
		AckWait:           5 * time.Minute,
		DLQSubject:        natsworker.DLQComplianceEraseSubject,
	}

	worker, err := natsworker.NewWorker(config, func(ctx context.Context, msg jetstream.Msg) error {
		return handleErase(ctx, svc, msg.Data())
	}, natsClient)
	if err != nil {
		return err
	}

	return worker.Start(ctx)
}

func handleErase(ctx context.Context, svc *coreprofiles.Service, data []byte) error {
	msg := &workercompliancev1.EraseMessage{}
	if err := proto.Unmarshal(data, msg); err != nil {
		slog.ErrorContext(ctx, "failed to unmarshal compliance erase message", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return natsworker.NewPermanentError(err).
			With("worker", "compliance-erase")
	}

	if err := protovalidate.Validate(msg); err != nil {
		slog.ErrorContext(ctx, "compliance erase message failed validation", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return natsworker.NewPermanentError(err).
			With("worker", "compliance-erase")
	}

	if err := svc.ExecuteErasure(ctx, msg.GetProjectId(), msg.GetRequestId()); err != nil {
		// A missing request row is unrecoverable — the message can never succeed,
		// so route it to the DLQ instead of retrying forever.
		if errors.Is(err, coreprofiles.ErrDeletionRequestNotFound) {
			return natsworker.NewPermanentError(err).
				With("worker", "compliance-erase").
				With("request_id", msg.GetRequestId()).
				With("project_id", msg.GetProjectId())
		}
		// Transient PG/CH failures: return for Nak/retry. Frozen identifiers keep
		// the retry correct even after events are deleted.
		return fmt.Errorf("execute erasure: %w", err)
	}
	return nil
}
