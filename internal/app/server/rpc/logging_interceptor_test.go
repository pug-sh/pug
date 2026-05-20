package rpc

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"github.com/pug-sh/pug/internal/apperr"
	"google.golang.org/protobuf/types/known/emptypb"
)

type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}
func (h *captureHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(_ string) slog.Handler      { return h }

func (h *captureHandler) last() slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.records[len(h.records)-1]
}

func callLogging(t *testing.T, handlerErr error) slog.Record {
	t.Helper()

	ch := &captureHandler{}
	old := slog.Default()
	slog.SetDefault(slog.New(ch))
	defer slog.SetDefault(old)

	interceptor := LoggingInterceptor()
	inner := interceptor.WrapUnary(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if handlerErr != nil {
			return nil, handlerErr
		}
		return nil, nil
	})

	req := connect.NewRequest(&emptypb.Empty{})
	_, _ = inner(context.Background(), req)

	return ch.last()
}

func TestLoggingInterceptor_Success(t *testing.T) {
	rec := callLogging(t, nil)
	if rec.Level != slog.LevelInfo {
		t.Errorf("level = %v, want Info", rec.Level)
	}
	if rec.Message != "rpc ok" {
		t.Errorf("message = %q, want %q", rec.Message, "rpc ok")
	}
}

func TestLoggingInterceptor_ClientError(t *testing.T) {
	rec := callLogging(t, connect.NewError(connect.CodeNotFound, errors.New("not found")))
	if rec.Level != slog.LevelWarn {
		t.Errorf("level = %v, want Warn", rec.Level)
	}
	if rec.Message != "rpc error" {
		t.Errorf("message = %q, want %q", rec.Message, "rpc error")
	}
}

func TestLoggingInterceptor_ServerError(t *testing.T) {
	rec := callLogging(t, connect.NewError(connect.CodeInternal, errors.New("db exploded")))
	if rec.Level != slog.LevelError {
		t.Errorf("level = %v, want Error", rec.Level)
	}
	if rec.Message != "rpc error" {
		t.Errorf("message = %q, want %q", rec.Message, "rpc error")
	}
}

func TestLoggingInterceptor_ValidationError(t *testing.T) {
	rec := callLogging(t, connect.NewError(connect.CodeInvalidArgument, errors.New("bad input")))
	if rec.Level != slog.LevelWarn {
		t.Errorf("level = %v, want Warn", rec.Level)
	}
}

func TestLoggingInterceptor_UnauthenticatedError(t *testing.T) {
	rec := callLogging(t, connect.NewError(connect.CodeUnauthenticated, errors.New("no token")))
	if rec.Level != slog.LevelWarn {
		t.Errorf("level = %v, want Warn", rec.Level)
	}
}

// isClientError must classify a raw *apperr.Error by its Code(), not via
// connect.CodeOf (which returns Unknown for apperr — it has a Code field, not a
// Code() method that satisfies Connect's coder interface). This keeps log levels
// correct even if LoggingInterceptor ever runs inside ErrorInterceptor.
func TestIsClientError_apperr(t *testing.T) {
	if !isClientError(apperr.NotFound(apperr.ReasonProfileNotFound, "x")) {
		t.Error("apperr.NotFound should classify as a client error (WARN)")
	}
	if !isClientError(apperr.Unauthenticated(apperr.ReasonUnauthenticated, "x")) {
		t.Error("apperr.Unauthenticated should classify as a client error (WARN)")
	}
	if isClientError(apperr.Err(connect.CodeInternal, apperr.ReasonInternal, "x")) {
		t.Error("apperr with CodeInternal must NOT classify as a client error (ERROR)")
	}
}

// TestLoggingInterceptor_apperrClientError pins the composed behavior: a handler
// returning a raw apperr.NotFound, run through Error+Logging in production order,
// logs at WARN.
func TestLoggingInterceptor_apperrClientError(t *testing.T) {
	ch := &captureHandler{}
	old := slog.Default()
	slog.SetDefault(slog.New(ch))
	defer slog.SetDefault(old)

	// Production order: Logging outermost, Error inside it.
	handler := func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		return nil, apperr.NotFound(apperr.ReasonProfileNotFound, "nope")
	}
	chain := LoggingInterceptor().WrapUnary(ErrorInterceptor().WrapUnary(handler))
	_, _ = chain(context.Background(), connect.NewRequest(&emptypb.Empty{}))

	if got := ch.last().Level; got != slog.LevelWarn {
		t.Errorf("level = %v, want Warn", got)
	}
}
