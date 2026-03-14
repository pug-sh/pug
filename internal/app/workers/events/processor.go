package events

import (
	"context"
	"log/slog"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	natsworker "github.com/fivebitsio/cotton/internal/deps/nats"
	eventsv1 "github.com/fivebitsio/cotton/internal/gen/proto/events/v1"
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
		return natsworker.NewPermanentError(err)
	}

	if len(batch.Events) == 0 {
		slog.WarnContext(ctx, "received empty event batch", slog.String("project_id", batch.ProjectId))
		return nil
	}

	// insert_time is omitted; ClickHouse fills it via DEFAULT now64(3).
	//
	// Deduplication: the events table uses ReplacingMergeTree(insert_time).
	// ClickHouse deduplicates rows whose ORDER BY key matches:
	//   (project_id, toStartOfMinute(occur_time), kind, event_id)
	// The row with the highest insert_time wins after a background merge.
	// Queries use SELECT ... FINAL to force dedup at read time.
	//
	// occur_time is part of the dedup key. Clients must send a stable
	// occur_time on retries — a different value that crosses an hour boundary
	// creates a new sort-key bucket and dedup will not fire. If occur_time
	// crosses a month boundary it also lands in a different partition
	// (PARTITION BY toYYYYMM(occur_time)), and ReplacingMergeTree never
	// deduplicates across partitions, producing permanent duplicates.
	//
	// If the client omits occur_time, the server defaults to time.Now() (see
	// below). A retry will produce a different time.Now(), which can cross
	// partition boundaries and break dedup. Clients should always send
	// occur_time explicitly.
	chBatch, err := p.ch.PrepareBatch(ctx, "INSERT INTO events (event_id, project_id, distinct_id, kind, auto_properties, custom_properties, occur_time)")
	if err != nil {
		slog.ErrorContext(ctx, "failed to prepare ClickHouse batch", slogx.Error(err), slog.String("project_id", batch.ProjectId), slog.Int("count", len(batch.Events)))
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
		ts := time.Now()
		if e.OccurTime != nil {
			ts = e.OccurTime.AsTime()
		}

		if err := chBatch.Append(
			e.EventId,
			batch.ProjectId,
			e.DistinctId,
			e.Kind,
			e.AutoProperties,
			e.CustomProperties,
			ts,
		); err != nil {
			slog.ErrorContext(ctx, "failed to append event to batch", slogx.Error(err), slog.String("project_id", batch.ProjectId), slog.Int("count", len(batch.Events)), slog.String("event_id", e.EventId), slog.Int("event_index", i))
			return natsworker.NewPermanentError(err)
		}
	}

	if err := chBatch.Send(); err != nil {
		slog.ErrorContext(ctx, "failed to send ClickHouse batch", slogx.Error(err), slog.String("project_id", batch.ProjectId), slog.Int("count", len(batch.Events)))
		return err
	}
	sent = true

	slog.InfoContext(ctx, "inserted events into ClickHouse",
		slog.String("project_id", batch.ProjectId),
		slog.Int("count", len(batch.Events)))

	return nil
}
