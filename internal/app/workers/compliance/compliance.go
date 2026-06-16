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
		return handleErase(ctx, svc, msg.Data(), isLastEraseDelivery(ctx, msg, config.MaxDeliver))
	}, natsClient)
	if err != nil {
		return err
	}

	return worker.Start(ctx)
}

// isLastEraseDelivery reports whether this is the final delivery before the
// worker framework dead-letters the message. It mirrors the framework's own
// last-delivery check so handleErase can record the failure on the ledger row
// before the message is terminated. Unreadable metadata is treated as the last
// delivery, matching the framework's conservative DLQ routing.
func isLastEraseDelivery(ctx context.Context, msg jetstream.Msg, maxDeliver int) bool {
	meta, err := msg.Metadata()
	if err != nil {
		slog.WarnContext(ctx, "failed reading erase message metadata; treating as last delivery", slogx.Error(err))
		return true
	}
	return int(meta.NumDelivered) >= maxDeliver
}

// erasureExecutor is the slice of the profiles service that handleErase drives,
// so the error classification + failure-recording can be unit-tested with a fake.
type erasureExecutor interface {
	ExecuteErasure(ctx context.Context, projectID, requestID string) error
	MarkErasureFailed(ctx context.Context, projectID, requestID string, cause error) error
}

func handleErase(ctx context.Context, svc erasureExecutor, data []byte, lastDelivery bool) error {
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
		// A missing request row is unrecoverable and there is no row to mark
		// failed — route straight to the DLQ instead of retrying forever.
		if errors.Is(err, coreprofiles.ErrDeletionRequestNotFound) {
			return permanentEraseError(err, msg)
		}
		// A request that resolves no identifiers can never succeed. The row exists,
		// so mark it failed, then route to the DLQ instead of retrying.
		if errors.Is(err, coreprofiles.ErrNoErasableIdentifiers) {
			markEraseFailed(ctx, svc, msg, err)
			return permanentEraseError(err, msg)
		}
		// Transient PG/CH failure: return for Nak/retry. Frozen identifiers keep
		// the retry correct even after events are deleted. On the final delivery
		// the framework dead-letters this message and never retries it, so record
		// the failure on the ledger row now — otherwise the DSAR audit trail is
		// stuck at 'processing' forever.
		if lastDelivery {
			markEraseFailed(ctx, svc, msg, err)
		}
		return fmt.Errorf("execute erasure: %w", err)
	}
	return nil
}

// permanentEraseError wraps err so the worker framework dead-letters the message
// (terminate, no retry), tagged with the subject for DLQ inspection.
func permanentEraseError(err error, msg *workercompliancev1.EraseMessage) error {
	return natsworker.NewPermanentError(err).
		With("worker", "compliance-erase").
		With("request_id", msg.GetRequestId()).
		With("project_id", msg.GetProjectId())
}

// markEraseFailed records the failure on the ledger row before the message is
// dead-lettered. The cause is already recorded at source; if the ledger write
// itself fails the row stays 'processing' until a re-request re-drives it, so
// this only logs the secondary failure.
func markEraseFailed(ctx context.Context, svc erasureExecutor, msg *workercompliancev1.EraseMessage, cause error) {
	if err := svc.MarkErasureFailed(ctx, msg.GetProjectId(), msg.GetRequestId(), cause); err != nil {
		slog.ErrorContext(ctx, "could not mark erasure failed before dead-lettering",
			slog.String("request_id", msg.GetRequestId()),
			slog.String("project_id", msg.GetProjectId()), slogx.Error(err))
	}
}
