package telemetry

import (
	"context"
	"log/slog"

	"github.com/pug-sh/pug/internal/correlation"
	"go.opentelemetry.io/otel/trace"
)

// correlationHandler stamps the per-request correlation id (and the trace id
// when a span is active) onto every log record, so all logs for a request
// correlate with the error_id returned to the client. No-ops when neither is set.
type correlationHandler struct{ inner slog.Handler }

func newCorrelationHandler(inner slog.Handler) slog.Handler {
	return &correlationHandler{inner: inner}
}

func (h *correlationHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *correlationHandler) Handle(ctx context.Context, r slog.Record) error {
	// error_id mirrors the key returned in API error responses, so a user-reported
	// id cross-references directly to these logs. trace_id is also stamped here for
	// text/stderr output (e.g. the shutdown fallback handler), which has no native
	// trace field. Because this handler wraps otelslog, the trace_id attr is also
	// emitted into OTLP, where it duplicates the SDK-set native trace id — harmless,
	// but not OTLP-free as the layering might suggest.
	if id := correlation.IDFromContext(ctx); id != "" {
		r.AddAttrs(slog.String("error_id", id))
	}
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()))
	}
	return h.inner.Handle(ctx, r)
}

func (h *correlationHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &correlationHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h *correlationHandler) WithGroup(name string) slog.Handler {
	return &correlationHandler{inner: h.inner.WithGroup(name)}
}
