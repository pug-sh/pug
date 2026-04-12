package telemetry

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestRecordError_NilError(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer tp.Shutdown(context.Background())

	ctx, span := tp.Tracer("test").Start(context.Background(), "test-span")
	RecordError(ctx, nil)
	span.End()

	spans := exporter.GetSpans()
	if len(spans) == 0 {
		t.Fatal("expected at least one span")
	}
	s := spans[0]
	if s.Status.Code != codes.Unset {
		t.Errorf("status = %v, want Unset for nil error", s.Status.Code)
	}
	if len(s.Events) != 0 {
		t.Errorf("expected no events for nil error, got %d", len(s.Events))
	}
}

func TestRecordError_RecordsError(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer tp.Shutdown(context.Background())

	ctx, span := tp.Tracer("test").Start(context.Background(), "test-span")
	RecordError(ctx, errors.New("something broke"))
	span.End()

	spans := exporter.GetSpans()
	if len(spans) == 0 {
		t.Fatal("expected at least one span")
	}
	s := spans[0]
	if s.Status.Code != codes.Error {
		t.Errorf("status = %v, want Error", s.Status.Code)
	}
	if s.Status.Description != "something broke" {
		t.Errorf("status description = %q, want %q", s.Status.Description, "something broke")
	}
	if len(s.Events) == 0 {
		t.Fatal("expected at least one event")
	}
	if s.Events[0].Name != "exception" {
		t.Errorf("event name = %q, want %q", s.Events[0].Name, "exception")
	}
}

func TestRecordError_WithAttributes(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer tp.Shutdown(context.Background())

	ctx, span := tp.Tracer("test").Start(context.Background(), "test-span")
	RecordError(ctx, errors.New("db error"), attribute.String("db.system", "postgresql"))
	span.End()

	spans := exporter.GetSpans()
	if len(spans) == 0 {
		t.Fatal("expected at least one span")
	}
	s := spans[0]
	if len(s.Events) == 0 {
		t.Fatal("expected at least one event")
	}

	found := false
	for _, a := range s.Events[0].Attributes {
		if string(a.Key) == "db.system" && a.Value.AsString() == "postgresql" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected db.system=postgresql attribute on error event")
	}
}
