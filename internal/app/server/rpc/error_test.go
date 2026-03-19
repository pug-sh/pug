package rpc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/emptypb"
)

type handlerFunc func(context.Context, *connect.Request[emptypb.Empty]) (*connect.Response[emptypb.Empty], error)

// testInterceptor exercises ErrorInterceptor via a real HTTP round-trip.
func testInterceptor(t *testing.T, ctx context.Context, handlerErr error) error {
	t.Helper()
	return testInterceptorWithOpts(t, ctx, nil, func(_ context.Context, _ *connect.Request[emptypb.Empty]) (*connect.Response[emptypb.Empty], error) {
		if handlerErr != nil {
			return nil, handlerErr
		}
		return connect.NewResponse(&emptypb.Empty{}), nil
	})
}

// testInterceptorWithOpts allows injecting HTTP middleware that wraps the Connect handler.
func testInterceptorWithOpts(t *testing.T, ctx context.Context, middleware func(http.Handler) http.Handler, handler handlerFunc) error {
	t.Helper()

	var h http.Handler = connect.NewUnaryHandler(
		"/test.v1.Svc/Method",
		handler,
		connect.WithInterceptors(ErrorInterceptor()),
	)
	if middleware != nil {
		h = middleware(h)
	}

	mux := http.NewServeMux()
	mux.Handle("/test.v1.Svc/Method", h)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client := connect.NewClient[emptypb.Empty, emptypb.Empty](
		srv.Client(),
		srv.URL+"/test.v1.Svc/Method",
	)

	_, err := client.CallUnary(ctx, connect.NewRequest(&emptypb.Empty{}))
	return err
}

func TestErrorInterceptor(t *testing.T) {
	t.Run("nil error passes through", func(t *testing.T) {
		err := testInterceptor(t, context.Background(), nil)
		if err != nil {
			t.Errorf("expected nil, got %v", err)
		}
	})

	t.Run("connect error passes through unchanged", func(t *testing.T) {
		orig := connect.NewError(connect.CodeNotFound, errors.New("not found"))
		err := testInterceptor(t, context.Background(), orig)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		var connectErr *connect.Error
		if !errors.As(err, &connectErr) {
			t.Fatalf("expected *connect.Error, got %T", err)
		}
		if connectErr.Code() != connect.CodeNotFound {
			t.Errorf("code = %v, want %v", connectErr.Code(), connect.CodeNotFound)
		}
		if connectErr.Message() != "not found" {
			t.Errorf("message = %q, want %q", connectErr.Message(), "not found")
		}
	})

	t.Run("plain error sanitized to internal", func(t *testing.T) {
		err := testInterceptor(t, context.Background(), errors.New("db connection refused"))
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		var connectErr *connect.Error
		if !errors.As(err, &connectErr) {
			t.Fatalf("expected *connect.Error, got %T", err)
		}
		if connectErr.Code() != connect.CodeInternal {
			t.Errorf("code = %v, want %v", connectErr.Code(), connect.CodeInternal)
		}
		if connectErr.Message() != "internal error" {
			t.Errorf("message = %q, want %q", connectErr.Message(), "internal error")
		}
	})

	t.Run("cancelled context returns canceled code", func(t *testing.T) {
		// Use HTTP middleware to cancel the request context before Connect
		// processes it. This ensures the interceptor's ctx.Err() check fires.
		cancelMiddleware := func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx, cancel := context.WithCancel(r.Context())
				cancel()
				next.ServeHTTP(w, r.WithContext(ctx))
			})
		}

		err := testInterceptorWithOpts(t, context.Background(), cancelMiddleware,
			func(_ context.Context, _ *connect.Request[emptypb.Empty]) (*connect.Response[emptypb.Empty], error) {
				return nil, errors.New("operation failed")
			})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		var connectErr *connect.Error
		if !errors.As(err, &connectErr) {
			t.Fatalf("expected *connect.Error, got %T", err)
		}
		if connectErr.Code() != connect.CodeCanceled {
			t.Errorf("code = %v, want %v", connectErr.Code(), connect.CodeCanceled)
		}
	})
}
