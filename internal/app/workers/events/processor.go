package events

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"buf.build/go/protovalidate"
	"github.com/ClickHouse/clickhouse-go/v2/lib/chcol"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"google.golang.org/protobuf/proto"

	natsworker "github.com/fivebitsio/cotton/internal/deps/nats"
	"github.com/fivebitsio/cotton/internal/deps/telemetry"
	commonv1 "github.com/fivebitsio/cotton/internal/gen/proto/common/v1"
	eventsv1 "github.com/fivebitsio/cotton/internal/gen/proto/sdk/events/v1"
	"github.com/fivebitsio/cotton/internal/slogx"
)

// propertyValueToVariant converts a proto PropertyValue oneof into a chcol.Variant
// tagged with the matching ClickHouse Variant branch name. Tagging is required:
// without it the driver dispatches by reflect type in column declaration order
// (String, Int64, Float64, Bool, DateTime64(3)), which can route values to the
// wrong slot — e.g. a float64 reaches the Int64 branch via reflect.CanConvert
// and gets truncated. Returns the zero Variant for unset/nil values, which the
// column treats as the absent-variant path (NULL).
//
// The returned chcol.Variant must stay aligned with both the column definition
// in 001_create_events_table.sql and the variantTypeToPropertyValueType mapping
// in insights/service.go — this alignment is asserted in variant_align_test.go.
func propertyValueToVariant(ctx context.Context, pv *commonv1.PropertyValue) chcol.Variant {
	if pv == nil {
		return chcol.Variant{}
	}
	switch v := pv.GetValue().(type) {
	case *commonv1.PropertyValue_StringValue:
		return chcol.NewVariantWithType(v.StringValue, "String")
	case *commonv1.PropertyValue_IntValue:
		return chcol.NewVariantWithType(v.IntValue, "Int64")
	case *commonv1.PropertyValue_DoubleValue:
		return chcol.NewVariantWithType(v.DoubleValue, "Float64")
	case *commonv1.PropertyValue_BoolValue:
		return chcol.NewVariantWithType(v.BoolValue, "Bool")
	case *commonv1.PropertyValue_TimestampValue:
		return chcol.NewVariantWithType(v.TimestampValue.AsTime().UTC().Truncate(time.Millisecond), "DateTime64(3)")
	default:
		// Unreachable for batches that pass protovalidate (oneof.required = true).
		// Fires only on proto-future drift: a new PropertyValue variant added in
		// proto without a corresponding case here. Surface the drift; the key
		// is preserved in the row but its value is stored as the absent-variant
		// slot (NULL on read), so the SDK still sees accepted=N back without
		// a per-property signal. Do not fail the batch.
		err := fmt.Errorf("unsupported PropertyValue variant: %T", pv.GetValue())
		slog.WarnContext(ctx, "storing property with unsupported PropertyValue variant as NULL", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return chcol.Variant{}
	}
}

// customPropertiesToVariantMap converts a proto Map(String, PropertyValue) into
// the typed Go shape clickhouse-go uses to insert into Map(String, Variant(...)).
// Returns nil for empty input — the driver maps a nil map to an empty CH Map
// (zero entries), which is the correct shape for an event with no custom_properties.
func customPropertiesToVariantMap(ctx context.Context, props map[string]*commonv1.PropertyValue) map[string]chcol.Variant {
	if len(props) == 0 {
		return nil
	}
	out := make(map[string]chcol.Variant, len(props))
	for k, v := range props {
		out[k] = propertyValueToVariant(ctx, v)
	}
	return out
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
		telemetry.RecordError(ctx, err)
		return natsworker.NewPermanentError(err).
			With("worker", "events")
	}

	if err := protovalidate.Validate(batch); err != nil {
		slog.ErrorContext(ctx, "event batch failed validation", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return natsworker.NewPermanentError(err).
			With("worker", "events")
	}

	if len(batch.Events) == 0 {
		slog.WarnContext(ctx, "received empty event batch", slog.String("project_id", batch.GetProjectId()))
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
		slog.ErrorContext(ctx, "failed to prepare ClickHouse batch", slogx.Error(err), slog.String("project_id", batch.GetProjectId()), slog.Int("count", len(batch.Events)))
		telemetry.RecordError(ctx, err)
		return err
	}

	sent := false
	defer func() {
		if !sent {
			if err := chBatch.Abort(); err != nil {
				slog.ErrorContext(ctx, "failed to abort ClickHouse batch", slogx.Error(err), slog.String("project_id", batch.GetProjectId()))
				telemetry.RecordError(ctx, err)
			}
		}
	}()

	for i, e := range batch.Events {
		if e.OccurTime == nil {
			err := fmt.Errorf("event[%d]: occur_time is required for dedup", i)
			slog.ErrorContext(ctx, "event missing required occur_time",
				slogx.Error(err),
				slog.String("project_id", batch.GetProjectId()),
				slog.String("event_id", e.GetEventId()),
				slog.Int("event_index", i))
			telemetry.RecordError(ctx, err)
			return natsworker.NewPermanentError(err).
				With("worker", "events").
				With("project_id", batch.GetProjectId()).
				With("event_id", e.GetEventId()).
				With("distinct_id", e.GetDistinctId()).
				With("kind", e.GetKind())
		}

		if err := chBatch.Append(
			e.GetEventId(),
			batch.GetProjectId(),
			e.GetDistinctId(),
			e.GetKind(),
			e.AutoProperties,
			customPropertiesToVariantMap(ctx, e.CustomProperties),
			e.OccurTime.AsTime(),
			e.GetSessionId(),
		); err != nil {
			slog.ErrorContext(ctx, "failed to append event to batch", slogx.Error(err), slog.String("project_id", batch.GetProjectId()), slog.Int("count", len(batch.Events)), slog.String("event_id", e.GetEventId()), slog.Int("event_index", i))
			telemetry.RecordError(ctx, err)
			return natsworker.NewPermanentError(err).
				With("worker", "events").
				With("project_id", batch.GetProjectId()).
				With("event_id", e.GetEventId())
		}
	}

	if err := chBatch.Send(); err != nil {
		slog.ErrorContext(ctx, "failed to send ClickHouse batch", slogx.Error(err), slog.String("project_id", batch.GetProjectId()), slog.Int("count", len(batch.Events)))
		telemetry.RecordError(ctx, err)
		return err
	}
	sent = true

	slog.InfoContext(ctx, "inserted events into ClickHouse",
		slog.String("project_id", batch.GetProjectId()),
		slog.Int("count", len(batch.Events)))

	return nil
}
