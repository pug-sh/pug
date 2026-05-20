package rpc

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"github.com/pug-sh/pug/internal/correlation"
	"github.com/rs/xid"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestCorrelationInterceptor_SetsID(t *testing.T) {
	var seen string
	next := connect.UnaryFunc(func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		seen = correlation.IDFromContext(ctx)
		return connect.NewResponse(&emptypb.Empty{}), nil
	})

	_, err := CorrelationInterceptor().WrapUnary(next)(context.Background(), connect.NewRequest(&emptypb.Empty{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := xid.FromString(seen); err != nil {
		t.Fatalf("expected a valid xid in context, got %q: %v", seen, err)
	}
}

func TestCorrelationInterceptor_SetsID_Streaming(t *testing.T) {
	var seen string
	next := connect.StreamingHandlerFunc(func(ctx context.Context, _ connect.StreamingHandlerConn) error {
		seen = correlation.IDFromContext(ctx)
		return nil
	})

	// The interceptor only augments ctx and passes conn through untouched, so a
	// nil conn is safe here.
	if err := CorrelationInterceptor().WrapStreamingHandler(next)(context.Background(), nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := xid.FromString(seen); err != nil {
		t.Fatalf("expected a valid xid in context, got %q: %v", seen, err)
	}
}
