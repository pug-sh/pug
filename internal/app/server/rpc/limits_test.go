package rpc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const limitsTestPath = "/test.v1.Svc/Method"

// reached distinguishes a rejection from a body that got through.
func serveWithLimits(t *testing.T, reached *bool, opts ...connect.HandlerOption) *httptest.Server {
	t.Helper()
	handler := connect.NewUnaryHandler(
		limitsTestPath,
		func(_ context.Context, _ *connect.Request[wrapperspb.StringValue]) (*connect.Response[emptypb.Empty], error) {
			*reached = true
			return connect.NewResponse(&emptypb.Empty{}), nil
		},
		opts...,
	)
	mux := http.NewServeMux()
	mux.Handle(limitsTestPath, handler)
	srv := httptest.NewServer(WithRequestLimits(mux))
	t.Cleanup(srv.Close)
	return srv
}

// No WithReadMaxBytes here, so only the middleware can reject this.
func TestWithRequestLimitsCapsWireBytes(t *testing.T) {
	var reached bool
	srv := serveWithLimits(t, &reached)
	client := connect.NewClient[wrapperspb.StringValue, emptypb.Empty](srv.Client(), srv.URL+limitsTestPath)

	payload := wrapperspb.String(strings.Repeat("x", MaxRequestBytes+1))
	_, err := client.CallUnary(context.Background(), connect.NewRequest(payload))
	if got := connect.CodeOf(err); got != connect.CodeResourceExhausted {
		t.Errorf("code = %v, want ResourceExhausted (err: %v)", got, err)
	}
	if reached {
		t.Error("handler ran on an over-sized body")
	}
}

// Gzipped, so it is tiny on the wire and only WithReadMaxBytes can reject it.
func TestWithRequestLimitsCapsDecompressedBytes(t *testing.T) {
	var reached bool
	srv := serveWithLimits(t, &reached, connect.WithReadMaxBytes(MaxRequestBytes))
	client := connect.NewClient[wrapperspb.StringValue, emptypb.Empty](
		srv.Client(), srv.URL+limitsTestPath, connect.WithSendGzip())

	payload := wrapperspb.String(strings.Repeat("x", MaxRequestBytes+1))
	_, err := client.CallUnary(context.Background(), connect.NewRequest(payload))
	if got := connect.CodeOf(err); got != connect.CodeResourceExhausted {
		t.Errorf("code = %v, want ResourceExhausted (err: %v)", got, err)
	}
	if reached {
		t.Error("handler ran on an over-sized decompressed message")
	}
}

func TestWithRequestLimitsAllowsNormalBody(t *testing.T) {
	var reached bool
	srv := serveWithLimits(t, &reached, connect.WithReadMaxBytes(MaxRequestBytes))
	client := connect.NewClient[wrapperspb.StringValue, emptypb.Empty](srv.Client(), srv.URL+limitsTestPath)

	payload := wrapperspb.String(strings.Repeat("x", 1<<20))
	if _, err := client.CallUnary(context.Background(), connect.NewRequest(payload)); err != nil {
		t.Fatalf("CallUnary: %v", err)
	}
	if !reached {
		t.Error("handler did not run on an in-size body")
	}
}

type deadlineRecorder struct {
	*httptest.ResponseRecorder
	read time.Time
}

func (r *deadlineRecorder) SetReadDeadline(t time.Time) error { r.read = t; return nil }

// ReadHeaderTimeout covers only headers; the body gets its own deadline.
func TestWithRequestLimitsSetsReadDeadline(t *testing.T) {
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	before := time.Now()
	WithRequestLimits(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))

	if rec.read.Before(before.Add(bodyReadTimeout)) {
		t.Errorf("read deadline = %v, want >= %v", rec.read, before.Add(bodyReadTimeout))
	}
}
