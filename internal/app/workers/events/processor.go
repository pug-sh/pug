package events

import (
	"context"
	"log/slog"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	natsworker "github.com/fivebitsio/cotton/internal/deps/nats"
	eventsv1 "github.com/fivebitsio/cotton/internal/gen/proto/events/v1"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/rs/xid"
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
		return &natsworker.PermanentError{Err: err}
	}

	if len(batch.Events) == 0 {
		slog.WarnContext(ctx, "received empty event batch", slog.String("project_id", batch.ProjectId))
		return nil
	}

	// insert_time is omitted; ClickHouse fills it via DEFAULT now64(3).
	chBatch, err := p.ch.PrepareBatch(ctx, "INSERT INTO events (id, project_id, distinct_id, kind, auto_properties, custom_properties, occur_time)")
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

	for _, e := range batch.Events {
		ts := time.Now()
		if e.OccurTime != nil {
			ts = e.OccurTime.AsTime()
		}

		if err := chBatch.Append(
			xid.New().String(),
			batch.ProjectId,
			e.DistinctId,
			e.Kind,
			e.AutoProperties,
			e.CustomProperties,
			ts,
		); err != nil {
			slog.ErrorContext(ctx, "failed to append event to batch", slogx.Error(err), slog.String("project_id", batch.ProjectId), slog.Int("count", len(batch.Events)))
			return err
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
