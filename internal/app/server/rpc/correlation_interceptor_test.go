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

func TestCorrelationInterceptor_reusesExistingID(t *testing.T) {
	// When an id is already in context (e.g. minted by WithCorrelationID outside
	// the Connect chain), the interceptor must reuse it, not overwrite it — so the
	// id an authn rejection used matches the one a successful request logs.
	var seen string
	next := connect.UnaryFunc(func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		seen = correlation.IDFromContext(ctx)
		return connect.NewResponse(&emptypb.Empty{}), nil
	})
	ctx := correlation.WithID(context.Background(), "preset-id")
	if _, err := CorrelationInterceptor().WrapUnary(next)(ctx, connect.NewRequest(&emptypb.Empty{})); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if seen != "preset-id" {
		t.Errorf("id = %q, want preset-id (must reuse an upstream-minted id)", seen)
	}
}

func TestCorrelationInterceptor_uniquePerRequest(t *testing.T) {
	ids := map[string]bool{}
	next := connect.UnaryFunc(func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		ids[correlation.IDFromContext(ctx)] = true
		return connect.NewResponse(&emptypb.Empty{}), nil
	})
	for range 3 {
		if _, err := CorrelationInterceptor().WrapUnary(next)(context.Background(), connect.NewRequest(&emptypb.Empty{})); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if len(ids) != 3 {
		t.Errorf("got %d distinct ids across 3 requests, want 3", len(ids))
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
