package rpc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"github.com/pug-sh/pug/internal/apperr"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
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
		connect.WithInterceptors(CorrelationInterceptor(), ErrorInterceptor()),
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

	t.Run("connect error code and message unchanged", func(t *testing.T) {
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

func allDetails(t *testing.T, err error) []any {
	t.Helper()
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		t.Fatalf("expected *connect.Error, got %T", err)
	}
	var out []any
	for _, d := range connectErr.Details() {
		msg, derr := d.Value()
		if derr != nil {
			continue
		}
		out = append(out, msg)
	}
	return out
}

func errorInfoFrom(t *testing.T, err error) *errdetails.ErrorInfo {
	t.Helper()
	for _, msg := range allDetails(t, err) {
		if info, ok := msg.(*errdetails.ErrorInfo); ok {
			return info
		}
	}
	return nil
}

func requestInfoFrom(t *testing.T, err error) *errdetails.RequestInfo {
	t.Helper()
	for _, msg := range allDetails(t, err) {
		if ri, ok := msg.(*errdetails.RequestInfo); ok {
			return ri
		}
	}
	return nil
}

func TestErrorInterceptor_attachesDetails(t *testing.T) {
	t.Run("plain connect error: ErrorInfo generic reason + RequestInfo id", func(t *testing.T) {
		err := testInterceptor(t, context.Background(),
			connect.NewError(connect.CodeNotFound, errors.New("nope")))
		info := errorInfoFrom(t, err)
		if info == nil {
			t.Fatal("no ErrorInfo detail attached")
		}
		if info.GetReason() != apperr.ReasonNotFound {
			t.Errorf("reason = %q, want %q", info.GetReason(), apperr.ReasonNotFound)
		}
		if info.GetDomain() != apperr.Domain {
			t.Errorf("domain = %q, want %q", info.GetDomain(), apperr.Domain)
		}
		ri := requestInfoFrom(t, err)
		if ri == nil || ri.GetRequestId() == "" {
			t.Errorf("RequestInfo = %+v, want non-empty request id", ri)
		}
	})

	t.Run("apperr error carries its specific reason", func(t *testing.T) {
		err := testInterceptor(t, context.Background(),
			apperr.Err(connect.CodeNotFound, apperr.ReasonProfileNotFound, "profile not found"))
		var connectErr *connect.Error
		if !errors.As(err, &connectErr) {
			t.Fatalf("expected *connect.Error, got %T", err)
		}
		if connectErr.Code() != connect.CodeNotFound {
			t.Errorf("code = %v, want NotFound", connectErr.Code())
		}
		if connectErr.Message() != "profile not found" {
			t.Errorf("message = %q", connectErr.Message())
		}
		info := errorInfoFrom(t, err)
		if info == nil || info.GetReason() != apperr.ReasonProfileNotFound {
			t.Errorf("ErrorInfo = %+v, want reason PROFILE_NOT_FOUND", info)
		}
		if ri := requestInfoFrom(t, err); ri == nil || ri.GetRequestId() == "" {
			t.Errorf("RequestInfo = %+v, want non-empty request id", ri)
		}
	})

	t.Run("leaked error sanitized to internal with INTERNAL reason", func(t *testing.T) {
		err := testInterceptor(t, context.Background(), errors.New("db boom"))
		info := errorInfoFrom(t, err)
		if info == nil || info.GetReason() != apperr.ReasonInternal {
			t.Errorf("ErrorInfo = %+v, want reason INTERNAL", info)
		}
	})
}

func TestErrorInterceptor_attachesDetailsToShortCircuitedError(t *testing.T) {
	// Models validate.NewInterceptor(): an inner interceptor that fails the request
	// before the handler runs. ErrorInterceptor (registered outside it) must still
	// attach ErrorInfo + RequestInfo.
	shortCircuit := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(_ context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid request"))
		}
	})

	h := connect.NewUnaryHandler(
		"/test.v1.Svc/Method",
		func(_ context.Context, _ *connect.Request[emptypb.Empty]) (*connect.Response[emptypb.Empty], error) {
			return connect.NewResponse(&emptypb.Empty{}), nil // never reached
		},
		connect.WithInterceptors(CorrelationInterceptor(), ErrorInterceptor(), shortCircuit),
	)
	mux := http.NewServeMux()
	mux.Handle("/test.v1.Svc/Method", h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client := connect.NewClient[emptypb.Empty, emptypb.Empty](srv.Client(), srv.URL+"/test.v1.Svc/Method")
	_, err := client.CallUnary(context.Background(), connect.NewRequest(&emptypb.Empty{}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	info := errorInfoFrom(t, err)
	if info == nil || info.GetReason() != apperr.ReasonInvalidArgument {
		t.Errorf("ErrorInfo = %+v, want reason INVALID_ARGUMENT", info)
	}
	if ri := requestInfoFrom(t, err); ri == nil || ri.GetRequestId() == "" {
		t.Errorf("RequestInfo = %+v, want non-empty request id", ri)
	}
}

func TestSanitizeError_ctxCancelNoDetail(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A raw (non-connect) error arriving with a cancelled context returns the
	// bare context error and attaches no detail (the ctx.Err() safety-net branch).
	err := sanitizeError(ctx, "/test.v1.Svc/Method", errors.New("boom"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if _, ok := errors.AsType[*connect.Error](err); ok {
		t.Errorf("expected raw context error (no connect error / no details), got %v", err)
	}
}
