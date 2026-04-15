package events

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	natsworker "github.com/fivebitsio/cotton/internal/deps/nats"
	"github.com/fivebitsio/cotton/internal/deps/telemetry"
	eventsv1 "github.com/fivebitsio/cotton/internal/gen/proto/sdk/events/v1"
	"github.com/fivebitsio/cotton/internal/slogx"
	"google.golang.org/protobuf/proto"
)

type Processor struct {
	ch driver.Conn
}

func NewProcessor(ch driver.Conn) *Processor {
	return &Processor{ch: ch}
}

func (p *Processor) ProcessMessage(ctx context.Context, data []byte) error {
	batch := &eventsv1.EventBatch{}
	if err := proto.Unmarshal(data, batch); err != nil {
		slog.ErrorContext(ctx, "failed to unmarshal event batch", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return natsworker.NewPermanentError(err).
			With("worker", "events")
	}

	if len(batch.Events) == 0 {
		slog.WarnContext(ctx, "received empty event batch", slog.String("project_id", batch.ProjectId))
		return nil
	}

	// insert_time is omitted; ClickHouse fills it via DEFAULT now64(3).
	//
	// Deduplication: the events table uses ReplacingMergeTree(insert_time),
	// which collapses rows with identical ORDER BY keys during background
	// merges, keeping the row with the highest insert_time. The ORDER BY key is:
	//   (project_id, toStartOfMinute(occur_time), kind, event_id)
	// Background merges collapse duplicates asynchronously; read queries rely
	// on eventual consistency rather than SELECT ... FINAL.
	//
	// As long as clients send the same occur_time on retries, the ORDER BY
	// key is identical and the retry is safely deduplicated regardless of
	// when it occurs.
	//
	// occur_time is required by proto validation (enforced in publisher.go
	// before events reach NATS). A different occur_time that crosses a minute
	// boundary creates a new sort-key bucket and dedup will not fire. If it
	// crosses a month boundary it also lands in a different partition
	// (PARTITION BY toYYYYMM(occur_time)), and ReplacingMergeTree never
	// deduplicates across partitions, producing permanent duplicates.
	chBatch, err := p.ch.PrepareBatch(ctx, "INSERT INTO events (event_id, project_id, distinct_id, kind, auto_properties, custom_properties, occur_time, session_id)")
	if err != nil {
		slog.ErrorContext(ctx, "failed to prepare ClickHouse batch", slogx.Error(err), slog.String("project_id", batch.ProjectId), slog.Int("count", len(batch.Events)))
		telemetry.RecordError(ctx, err)
		return err
	}

	sent := false
	defer func() {
		if !sent {
			if err := chBatch.Abort(); err != nil {
				slog.ErrorContext(ctx, "failed to abort ClickHouse batch", slogx.Error(err), slog.String("project_id", batch.ProjectId))
			}
		}
	}()

	for i, e := range batch.Events {
		if e.OccurTime == nil {
			slog.ErrorContext(ctx, "event missing required occur_time",
				slog.String("project_id", batch.ProjectId),
				slog.String("event_id", e.EventId),
				slog.Int("event_index", i))
			return natsworker.NewPermanentError(fmt.Errorf("event[%d]: occur_time is required for dedup", i)).
				With("worker", "events").
				With("project_id", batch.ProjectId).
				With("event_id", e.EventId).
				With("distinct_id", e.DistinctId).
				With("kind", e.Kind)
		}

		if err := chBatch.Append(
			e.EventId,
			batch.ProjectId,
			e.DistinctId,
			e.Kind,
			e.AutoProperties,
			e.CustomProperties,
			e.OccurTime.AsTime(),
			e.SessionId,
		); err != nil {
			slog.ErrorContext(ctx, "failed to append event to batch", slogx.Error(err), slog.String("project_id", batch.ProjectId), slog.Int("count", len(batch.Events)), slog.String("event_id", e.EventId), slog.Int("event_index", i))
			telemetry.RecordError(ctx, err)
			return natsworker.NewPermanentError(err).
				With("worker", "events").
				With("project_id", batch.ProjectId).
				With("event_id", e.EventId)
		}
	}

	if err := chBatch.Send(); err != nil {
		slog.ErrorContext(ctx, "failed to send ClickHouse batch", slogx.Error(err), slog.String("project_id", batch.ProjectId), slog.Int("count", len(batch.Events)))
		telemetry.RecordError(ctx, err)
		return err
	}
	sent = true

	slog.InfoContext(ctx, "inserted events into ClickHouse",
		slog.String("project_id", batch.ProjectId),
		slog.Int("count", len(batch.Events)))

	return nil
}
