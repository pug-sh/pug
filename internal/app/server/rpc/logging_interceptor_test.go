package rpc

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"connectrpc.com/connect"
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
func (h *captureHandler) WithGroup(_ string) slog.Handler       { return h }

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
