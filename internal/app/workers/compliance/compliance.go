package compliance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"buf.build/go/protovalidate"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/pug-sh/pug/internal/core/deletion"
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

// The compliance worker hosts slow, low-volume jobs that share long processing
// timeouts. Profile erasure and project purge use separate consumers so each can
// keep its own delivery policy while sharing the existing compliance stream.

func Run(ctx context.Context) error {
	closeOtel, err := telemetry.SetupSDK(ctx)
	if err != nil {
		return err
	}
	defer telemetry.ShutdownOnExit(ctx, closeOtel)

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
	var purgeReplicas []driver.Conn
	for raw := range strings.SplitSeq(strings.TrimSpace(os.Getenv("PUG_CLICKHOUSE_PURGE_REPLICA_URLS")), ",") {
		url := strings.TrimSpace(raw)
		if url == "" {
			continue
		}
		replicaDB, err := clickhouse.NewFromConfig(ctx, &clickhouse.Config{URL: url})
		if err != nil {
			return fmt.Errorf("connect deletion purge replica: %w", err)
		}
		defer replicaDB.Close(context.WithoutCancel(ctx))
		purgeReplicas = append(purgeReplicas, replicaDB.Conn)
	}

	slog.InfoContext(ctx, "Starting compliance worker...")
	return StartWorker(ctx, pgW, chDB.Conn, natsClient, purgeReplicas...)
}

func StartWorker(ctx context.Context, pgW *pgxpool.Pool, ch driver.Conn, natsClient *natsworker.NATSClient, purgeReplicas ...driver.Conn) error {
	svc := coreprofiles.NewService(pgW, ch, natsClient)

	// One process, one consumer per compliance job. Retention (§4.5) slots in as an
	// additional g.Go(...) consumer here. Export (§4.2) stays out of the worker per
	// 4.2 decision 2 — it needs no async job.
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return runEraseConsumer(ctx, svc, natsClient) })
	purgeCH := ch
	if len(purgeReplicas) > 0 {
		purgeCH = purgeReplicas[0]
		purgeReplicas = purgeReplicas[1:]
	}
	purgeExec := &projectPurgeExecutor{
		purger: deletion.NewPurger(pgW, purgeCH, purgeReplicas...),
		ledger: deletion.NewService(pgW),
	}
	g.Go(func() error { return runProjectPurgeConsumer(ctx, purgeExec, natsClient) })
	return g.Wait()
}

type projectPurgeExecutor struct {
	purger *deletion.Purger
	ledger *deletion.Service
}

func (e *projectPurgeExecutor) ProcessOperation(ctx context.Context, operationID string) error {
	return e.purger.ProcessOperation(ctx, operationID)
}

func (e *projectPurgeExecutor) MarkFailed(ctx context.Context, operationID string, cause error) error {
	return e.ledger.MarkFailed(ctx, operationID, cause)
}

func runProjectPurgeConsumer(ctx context.Context, exec purgeExecutor, natsClient *natsworker.NATSClient) error {
	consumerConfig, err := natsClient.GetConsumerConfigByName("compliance-project-purge-processor-durable")
	if err != nil {
		return fmt.Errorf("failed to get project purge consumer config: %w", err)
	}
	config := natsworker.WorkerConfig{
		StreamName:        consumerConfig.StreamName,
		ConsumerName:      consumerConfig.DurableName,
		DurableName:       consumerConfig.DurableName,
		FilterSubject:     consumerConfig.FilterSubject,
		Concurrency:       1,
		ProcessingTimeout: 30 * time.Minute,
		MaxDeliver:        consumerConfig.MaxDeliver,
		AckWait:           30 * time.Minute,
		DLQSubject:        natsworker.DLQComplianceProjectPurgeSubject,
	}
	worker, err := natsworker.NewWorker(config, func(ctx context.Context, msg jetstream.Msg) error {
		return handleProjectPurge(ctx, exec, msg.Data(), isLastDelivery(ctx, msg, config.MaxDeliver))
	}, natsClient)
	if err != nil {
		return err
	}
	return worker.Start(ctx)
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
		return handleErase(ctx, svc, msg.Data(), isLastDelivery(ctx, msg, config.MaxDeliver))
	}, natsClient)
	if err != nil {
		return err
	}

	return worker.Start(ctx)
}

// isLastDelivery reports whether this is the final delivery before the worker
// framework dead-letters the message. It mirrors the framework's own check so a
// handler can record a terminal failure on its ledger before termination.
func isLastDelivery(ctx context.Context, msg jetstream.Msg, maxDeliver int) bool {
	meta, err := msg.Metadata()
	if err != nil {
		slog.WarnContext(ctx, "failed reading message metadata; treating as last delivery", slogx.Error(err))
		return true
	}
	return int(meta.NumDelivered) >= maxDeliver
}

type purgeExecutor interface {
	ProcessOperation(context.Context, string) error
	MarkFailed(context.Context, string, error) error
}

func handleProjectPurge(ctx context.Context, exec purgeExecutor, data []byte, lastDelivery bool) error {
	msg := &workercompliancev1.ProjectPurgeMessage{}
	if err := proto.Unmarshal(data, msg); err != nil {
		slog.ErrorContext(ctx, "failed to unmarshal project purge message", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return natsworker.NewPermanentError(err).With("worker", "compliance-project-purge")
	}
	if err := protovalidate.Validate(msg); err != nil {
		slog.ErrorContext(ctx, "project purge message failed validation", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return natsworker.NewPermanentError(err).With("worker", "compliance-project-purge")
	}
	err := exec.ProcessOperation(ctx, msg.GetOperationId())
	if err == nil {
		return nil
	}
	if notDue, ok := errors.AsType[*deletion.NotDueError](err); ok {
		return natsworker.DeferFor(max(notDue.Delay, time.Millisecond))
	}
	if errors.Is(err, deletion.ErrNotFound) {
		slog.ErrorContext(ctx, "project purge operation does not exist",
			slog.String("operation_id", msg.GetOperationId()), slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return natsworker.NewPermanentError(err).
			With("worker", "compliance-project-purge").
			With("operation_id", msg.GetOperationId())
	}
	slog.ErrorContext(ctx, "project purge operation failed",
		slog.String("operation_id", msg.GetOperationId()), slogx.Error(err))
	telemetry.RecordError(ctx, err)
	if lastDelivery {
		if markErr := exec.MarkFailed(ctx, msg.GetOperationId(), err); markErr != nil {
			slog.ErrorContext(ctx, "could not mark project purge failed before dead-lettering",
				slog.String("operation_id", msg.GetOperationId()), slogx.Error(markErr))
			telemetry.RecordError(ctx, markErr)
		}
	}
	return fmt.Errorf("execute project purge: %w", err)
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
// this logs the secondary failure and goes no further.
func markEraseFailed(ctx context.Context, svc erasureExecutor, msg *workercompliancev1.EraseMessage, cause error) {
	if err := svc.MarkErasureFailed(ctx, msg.GetProjectId(), msg.GetRequestId(), cause); err != nil {
		slog.ErrorContext(ctx, "could not mark erasure failed before dead-lettering",
			slog.String("request_id", msg.GetRequestId()),
			slog.String("project_id", msg.GetProjectId()),
			slogx.Error(err)) // puglint:exempt — MarkErasureFailed recorded this write failure
	}
}
