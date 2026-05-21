package telemetry

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/pug-sh/pug/internal/correlation"
	"go.opentelemetry.io/otel/trace"
)

// capturingHandler records the last handled record for assertions.
type capturingHandler struct{ rec slog.Record }

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.rec = r
	return nil
}
func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }

func attrValue(r slog.Record, key string) (string, bool) {
	var out string
	var found bool
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			out, found = a.Value.String(), true
			return false
		}
		return true
	})
	return out, found
}

func TestCorrelationHandler_stampsID(t *testing.T) {
	cap := &capturingHandler{}
	h := newCorrelationHandler(cap)
	ctx := correlation.WithID(context.Background(), "id-xyz")

	rec := slog.NewRecord(time.Time{}, slog.LevelInfo, "msg", 0)
	if err := h.Handle(ctx, rec); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, ok := attrValue(cap.rec, "error_id")
	if !ok || got != "id-xyz" {
		t.Errorf("error_id attr = %q (found=%v), want id-xyz", got, ok)
	}
}

func TestCorrelationHandler_noIDNoAttr(t *testing.T) {
	cap := &capturingHandler{}
	h := newCorrelationHandler(cap)
	rec := slog.NewRecord(time.Time{}, slog.LevelInfo, "msg", 0)
	if err := h.Handle(context.Background(), rec); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, ok := attrValue(cap.rec, "error_id"); ok {
		t.Error("error_id attr present but no id was set")
	}
}

func TestCorrelationHandler_stampsTraceID(t *testing.T) {
	cap := &capturingHandler{}
	h := newCorrelationHandler(cap)
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10},
		SpanID:  trace.SpanID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	rec := slog.NewRecord(time.Time{}, slog.LevelInfo, "msg", 0)
	if err := h.Handle(ctx, rec); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, ok := attrValue(cap.rec, "trace_id")
	if !ok || got != sc.TraceID().String() {
		t.Errorf("trace_id attr = %q (found=%v), want %q", got, ok, sc.TraceID().String())
	}
}

func TestCorrelationHandler_stampsIDAndTraceTogether(t *testing.T) {
	// The production path: a traced request with a correlation id stamps both attrs.
	cap := &capturingHandler{}
	h := newCorrelationHandler(cap)
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10},
		SpanID:  trace.SpanID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
	})
	ctx := trace.ContextWithSpanContext(correlation.WithID(context.Background(), "id-both"), sc)

	rec := slog.NewRecord(time.Time{}, slog.LevelInfo, "msg", 0)
	if err := h.Handle(ctx, rec); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got, ok := attrValue(cap.rec, "error_id"); !ok || got != "id-both" {
		t.Errorf("error_id = %q (found=%v), want id-both", got, ok)
	}
	if got, ok := attrValue(cap.rec, "trace_id"); !ok || got != sc.TraceID().String() {
		t.Errorf("trace_id = %q (found=%v), want %q", got, ok, sc.TraceID().String())
	}
}

func TestCorrelationHandler_stampsAfterWithGroup(t *testing.T) {
	// The wrapper must survive WithGroup and still stamp the id.
	cap := &capturingHandler{}
	h := newCorrelationHandler(cap).WithGroup("grp")
	ctx := correlation.WithID(context.Background(), "id-grp")

	rec := slog.NewRecord(time.Time{}, slog.LevelInfo, "msg", 0)
	if err := h.Handle(ctx, rec); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got, ok := attrValue(cap.rec, "error_id"); !ok || got != "id-grp" {
		t.Errorf("error_id after WithGroup = %q (found=%v), want id-grp", got, ok)
	}
}

func TestCorrelationHandler_stampsAfterWithAttrs(t *testing.T) {
	cap := &capturingHandler{}
	// The wrapper must survive WithAttrs and still stamp the id.
	h := newCorrelationHandler(cap).WithAttrs([]slog.Attr{slog.String("base", "x")})
	ctx := correlation.WithID(context.Background(), "id-attrs")

	rec := slog.NewRecord(time.Time{}, slog.LevelInfo, "msg", 0)
	if err := h.Handle(ctx, rec); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got, ok := attrValue(cap.rec, "error_id"); !ok || got != "id-attrs" {
		t.Errorf("error_id after WithAttrs = %q (found=%v), want id-attrs", got, ok)
	}
}
