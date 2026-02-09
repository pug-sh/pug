package events

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	eventsv1 "github.com/fivebitsio/cotton/internal/gen/proto/events/v1"
	"github.com/fivebitsio/cotton/internal/slogx"
	"github.com/rs/xid"
	"google.golang.org/protobuf/proto"
)

// PermanentError wraps errors that should not be retried.
type PermanentError struct {
	Err error
}

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// IsPermanentError checks if an error is a PermanentError.
func IsPermanentError(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}

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
		return &PermanentError{Err: err}
	}

	if len(batch.Events) == 0 {
		return nil
	}

	chBatch, err := p.ch.PrepareBatch(ctx, "INSERT INTO events (id, project_id, distinct_id, event, sdk_properties, user_properties, event_time)")
	if err != nil {
		slog.ErrorContext(ctx, "failed to prepare ClickHouse batch", slogx.Error(err))
		return err
	}

	sent := false
	defer func() {
		if !sent {
			chBatch.Abort()
		}
	}()

	for _, e := range batch.Events {
		ts := time.Now()
		if e.EventTime != nil {
			ts = e.EventTime.AsTime()
		}

		if err := chBatch.Append(
			xid.New().String(),
			batch.ProjectId,
			e.DistinctId,
			e.Event,
			e.SdkProperties,
			e.UserProperties,
			ts,
		); err != nil {
			slog.ErrorContext(ctx, "failed to append event to batch", slogx.Error(err))
			return err
		}
	}

	if err := chBatch.Send(); err != nil {
		slog.ErrorContext(ctx, "failed to send ClickHouse batch", slogx.Error(err))
		return err
	}
	sent = true

	slog.Info("inserted events into ClickHouse",
		slog.String("project_id", batch.ProjectId),
		slog.Int("count", len(batch.Events)))

	return nil
}
