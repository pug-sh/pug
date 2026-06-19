package nats

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// The dlqMessageCounter is created at package init() against the global meter.
// OTel's global instruments delegate to the real provider once one is installed,
// so installing a ManualReader-backed provider here (once, before the first
// measurement) lets these tests read what handleMessage records. The provider is
// process-global and the counter accumulates, so tests measure the delta of a
// specific (disposition, outcome) data point rather than its absolute value.
var (
	testMeterOnce   sync.Once
	testMeterReader *sdkmetric.ManualReader
)

func metricReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	testMeterOnce.Do(func() {
		testMeterReader = sdkmetric.NewManualReader()
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(testMeterReader)))
	})
	return testMeterReader
}

// dlqCounterValue returns the current nats.dlq_messages_total value for the
// (disposition, outcome) attribute pair, or 0 if that data point has not been
// recorded yet.
func dlqCounterValue(t *testing.T, reader *sdkmetric.ManualReader, disposition, outcome string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "nats.dlq_messages_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("nats.dlq_messages_total has data type %T, want metricdata.Sum[int64]", m.Data)
			}
			for _, dp := range sum.DataPoints {
				d, _ := dp.Attributes.Value(attribute.Key("disposition"))
				o, _ := dp.Attributes.Value(attribute.Key("outcome"))
				if d.AsString() == disposition && o.AsString() == outcome {
					return dp.Value
				}
			}
		}
	}
	return 0
}

// A retryable failure whose metadata cannot be read is dead-lettered (treated as
// last delivery) but must NOT be labeled disposition="max_deliver": it did not
// exhaust its retries, the metadata was simply unavailable. Operators alerting on
// outcome=dropped need to distinguish "burned all retries" from "couldn't read
// metadata," so this path records disposition="metadata_unavailable".
func TestHandleMessageMetadataUnavailableDisposition(t *testing.T) {
	reader := metricReader(t)
	before := dlqCounterValue(t, reader, dispositionMetadataUnavailable, outcomePublished)

	m := &fakeMsg{subject: "s.sub", metaErr: errors.New("no metadata")}
	js := &fakeJetStream{}
	w := newTestWorker(failProc, js)

	w.handleMessage(context.Background(), m)

	after := dlqCounterValue(t, reader, dispositionMetadataUnavailable, outcomePublished)
	if after-before != 1 {
		t.Fatalf("disposition=%q outcome=%q delta = %d, want 1",
			dispositionMetadataUnavailable, outcomePublished, after-before)
	}
}

// outcomeLabel maps the publish result to the metric's outcome label. This is the
// single point where the data-loss polarity is decided, so it is pinned directly:
// a published dead-letter is "published"; a failed publish (lost message) is
// "dropped". Inverting these would make the data-loss alert fire on success.
func TestOutcomeLabel(t *testing.T) {
	if got := outcomeLabel(true); got != outcomePublished {
		t.Errorf("outcomeLabel(true) = %q, want %q", got, outcomePublished)
	}
	if got := outcomeLabel(false); got != outcomeDropped {
		t.Errorf("outcomeLabel(false) = %q, want %q", got, outcomeDropped)
	}
}

// The DLQ metric must actually be emitted from handleMessage on each routing path
// with the right (disposition, outcome) pair — it is the only machine-readable
// signal for dead-letter routing and data loss, so end-to-end emission is pinned,
// not just the label helper.
func TestDLQMetricEmission(t *testing.T) {
	reader := metricReader(t)

	t.Run("exhausted retries published", func(t *testing.T) {
		before := dlqCounterValue(t, reader, dispositionExhausted, outcomePublished)
		m := &fakeMsg{subject: "s.sub", meta: &jetstream.MsgMetadata{NumDelivered: 3}}
		newTestWorker(failProc, &fakeJetStream{}).handleMessage(context.Background(), m)
		if d := dlqCounterValue(t, reader, dispositionExhausted, outcomePublished) - before; d != 1 {
			t.Fatalf("max_deliver/published delta = %d, want 1", d)
		}
	})

	t.Run("dlq publish failure dropped", func(t *testing.T) {
		before := dlqCounterValue(t, reader, dispositionExhausted, outcomeDropped)
		m := &fakeMsg{subject: "s.sub", meta: &jetstream.MsgMetadata{NumDelivered: 3}}
		js := &fakeJetStream{publishErr: errors.New("dlq stream down")}
		newTestWorker(failProc, js).handleMessage(context.Background(), m)
		if d := dlqCounterValue(t, reader, dispositionExhausted, outcomeDropped) - before; d != 1 {
			t.Fatalf("max_deliver/dropped delta = %d, want 1", d)
		}
	})

	t.Run("permanent error published", func(t *testing.T) {
		before := dlqCounterValue(t, reader, dispositionPermanent, outcomePublished)
		m := &fakeMsg{subject: "s.sub", meta: &jetstream.MsgMetadata{NumDelivered: 1}}
		permErr := NewPermanentError(errors.New("poison"))
		proc := func(context.Context, jetstream.Msg) error { return permErr }
		newTestWorker(proc, &fakeJetStream{}).handleMessage(context.Background(), m)
		if d := dlqCounterValue(t, reader, dispositionPermanent, outcomePublished) - before; d != 1 {
			t.Fatalf("permanent/published delta = %d, want 1", d)
		}
	})
}
