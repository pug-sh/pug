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
	// id cross-references directly to these logs. trace_id is stamped as a record
	// attr because in stdout mode (no OTLP endpoint configured) the inner handler is
	// a plain slog text handler with no native trace correlation, so this attr is the
	// only way the trace id reaches the log. Under the otelslog inner handler (OTLP
	// mode) it is redundant with the SDK-set native trace id — harmless. (The
	// shutdown fallback text handler in otel.go is not wrapped by this handler, so it
	// does not receive these attrs.)
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
