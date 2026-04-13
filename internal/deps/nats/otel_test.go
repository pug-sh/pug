package nats

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func setupOTel(t *testing.T) (*tracetest.InMemoryExporter, trace.TracerProvider) {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))

	oldTP := otel.GetTracerProvider()
	oldProp := otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	t.Cleanup(func() {
		otel.SetTracerProvider(oldTP)
		otel.SetTextMapPropagator(oldProp)
		_ = tp.Shutdown(context.Background())
	})
	return exporter, tp
}

func TestHeaderCarrier_SetGet(t *testing.T) {
	h := make(nats.Header)
	c := headerCarrier(h)

	c.Set("traceparent", "00-abc123-def456-01")
	got := c.Get("traceparent")
	if got != "00-abc123-def456-01" {
		t.Errorf("Get = %q, want %q", got, "00-abc123-def456-01")
	}
}

func TestHeaderCarrier_Keys(t *testing.T) {
	h := make(nats.Header)
	h.Set("traceparent", "val1")
	h.Set("tracestate", "val2")
	c := headerCarrier(h)

	keys := c.Keys()
	if len(keys) != 2 {
		t.Fatalf("got %d keys, want 2", len(keys))
	}

	keySet := make(map[string]bool)
	for _, k := range keys {
		keySet[k] = true
	}
	// nats.Header normalizes keys to lowercase.
	if !keySet["traceparent"] && !keySet["Traceparent"] {
		t.Errorf("keys = %v, missing traceparent", keys)
	}
	if !keySet["tracestate"] && !keySet["Tracestate"] {
		t.Errorf("keys = %v, missing tracestate", keys)
	}
}

func TestTraceContext_InjectExtractRoundtrip(t *testing.T) {
	_, tp := setupOTel(t)

	ctx, span := tp.Tracer("test").Start(context.Background(), "producer")
	origTraceID := span.SpanContext().TraceID()

	msg := &nats.Msg{Header: make(nats.Header)}
	injectTraceContext(ctx, msg)
	span.End()

	if msg.Header.Get("traceparent") == "" {
		t.Fatal("expected traceparent header to be set")
	}

	extracted := extractTraceContext(context.Background(), &stubMsg{headers: msg.Header})

	extractedSpan := trace.SpanFromContext(extracted)
	if extractedSpan.SpanContext().TraceID() != origTraceID {
		t.Errorf("trace ID = %v, want %v", extractedSpan.SpanContext().TraceID(), origTraceID)
	}
	if !extractedSpan.SpanContext().IsRemote() {
		t.Error("expected remote span context after extraction")
	}
}

func TestInjectTraceContext_NoSpan(t *testing.T) {
	setupOTel(t)

	msg := &nats.Msg{Header: make(nats.Header)}
	injectTraceContext(context.Background(), msg)

	if tp := msg.Header.Get("Traceparent"); tp != "" {
		t.Errorf("expected no Traceparent without active span, got %q", tp)
	}
}

func TestStartProducerSpan(t *testing.T) {
	exporter, _ := setupOTel(t)

	_, span := startProducerSpan(context.Background(), "events.ingest", 128)
	span.End()

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	if spans[0].Name != "send events.ingest" {
		t.Errorf("span name = %q, want %q", spans[0].Name, "send events.ingest")
	}
	if spans[0].SpanKind != trace.SpanKindProducer {
		t.Errorf("span kind = %v, want Producer", spans[0].SpanKind)
	}

	attrs := make(map[string]any)
	for _, a := range spans[0].Attributes {
		attrs[string(a.Key)] = a.Value.AsInterface()
	}
	if attrs["messaging.system"] != "nats" {
		t.Errorf("messaging.system = %v, want nats", attrs["messaging.system"])
	}
	if attrs["messaging.destination.name"] != "events.ingest" {
		t.Errorf("messaging.destination.name = %v, want events.ingest", attrs["messaging.destination.name"])
	}
}

func TestStartConsumerSpan(t *testing.T) {
	exporter, _ := setupOTel(t)

	_, span := startConsumerSpan(context.Background(), "events.ingest", "EVENTS", "events-writer", 1, 42, 7)
	span.End()

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	if spans[0].Name != "process events.ingest" {
		t.Errorf("span name = %q, want %q", spans[0].Name, "process events.ingest")
	}
	if spans[0].SpanKind != trace.SpanKindConsumer {
		t.Errorf("span kind = %v, want Consumer", spans[0].SpanKind)
	}
}

// stubMsg implements jetstream.Msg for testing.
type stubMsg struct {
	headers nats.Header
}

func (m *stubMsg) Metadata() (*jetstream.MsgMetadata, error) { return nil, nil }
func (m *stubMsg) Data() []byte                              { return nil }
func (m *stubMsg) Headers() nats.Header                      { return m.headers }
func (m *stubMsg) Subject() string                           { return "" }
func (m *stubMsg) Reply() string                             { return "" }
func (m *stubMsg) Ack() error                                { return nil }
func (m *stubMsg) DoubleAck(_ context.Context) error         { return nil }
func (m *stubMsg) Nak() error                                { return nil }
func (m *stubMsg) NakWithDelay(_ time.Duration) error        { return nil }
func (m *stubMsg) InProgress() error                         { return nil }
func (m *stubMsg) Term() error                               { return nil }
func (m *stubMsg) TermWithReason(_ string) error             { return nil }
