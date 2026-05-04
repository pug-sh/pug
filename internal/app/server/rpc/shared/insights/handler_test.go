package insights

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/authn"
	"connectrpc.com/connect"

	"github.com/pug-sh/pug/internal/app/server/rpc"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"

	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
)

func TestQuery_Unauthenticated(t *testing.T) {
	s := &server{}
	_, err := s.Query(context.Background(), connect.NewRequest(&insightsv1.QueryRequest{}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if code := connect.CodeOf(err); code != connect.CodeUnauthenticated {
		t.Errorf("got code %v, want CodeUnauthenticated", code)
	}
}

func TestSegmentUsers_Unauthenticated(t *testing.T) {
	s := &server{}
	_, err := s.SegmentUsers(context.Background(), connect.NewRequest(&insightsv1.SegmentUsersRequest{}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if code := connect.CodeOf(err); code != connect.CodeUnauthenticated {
		t.Errorf("got code %v, want CodeUnauthenticated", code)
	}
}

func TestGetFilterSchema_Unauthenticated(t *testing.T) {
	s := &server{}
	_, err := s.GetFilterSchema(context.Background(), connect.NewRequest(&commonv1.GetFilterSchemaRequest{}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if code := connect.CodeOf(err); code != connect.CodeUnauthenticated {
		t.Errorf("got code %v, want CodeUnauthenticated", code)
	}
}

func TestGetPropertyValues_Unauthenticated(t *testing.T) {
	s := &server{}
	_, err := s.GetPropertyValues(context.Background(), connect.NewRequest(&insightsv1.GetPropertyValuesRequest{}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if code := connect.CodeOf(err); code != connect.CodeUnauthenticated {
		t.Errorf("got code %v, want CodeUnauthenticated", code)
	}
}

func TestConnectCtxErr_Canceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := connectCtxErr(ctx.Err())
	if code := connect.CodeOf(err); code != connect.CodeCanceled {
		t.Errorf("got code %v, want CodeCanceled", code)
	}
}

func TestConnectCtxErr_DeadlineExceeded(t *testing.T) {
	err := connectCtxErr(context.DeadlineExceeded)
	if code := connect.CodeOf(err); code != connect.CodeDeadlineExceeded {
		t.Errorf("got code %v, want CodeDeadlineExceeded", code)
	}
}

func TestQuery_CanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &server{}
	_, err := s.Query(ctx, connect.NewRequest(&insightsv1.QueryRequest{}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if code := connect.CodeOf(err); code != connect.CodeCanceled {
		t.Errorf("got code %v, want CodeCanceled", code)
	}
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		t.Fatal("expected connect.Error")
	}
	if connectErr.Message() != "request canceled" {
		t.Errorf("got message %q, want %q", connectErr.Message(), "request canceled")
	}
}

// TestQuery_UnsupportedInsightType verifies the switch default arm: a known-bad enum value
// (proto/Go drift, not a real client value) is treated as a server-side bug and translated
// to CodeInternal. The handler reaches this path only via drift because protovalidate
// rejects unknown enum values at the interceptor; this test bypasses the interceptor by
// calling the handler directly. Locks in the contract that drift produces CodeInternal,
// not CodeInvalidArgument.
func TestQuery_UnsupportedInsightType(t *testing.T) {
	ctx := authn.SetInfo(context.Background(), &rpc.Principal{
		Project: &dbread.Project{ID: "test-project"},
	})
	s := &server{}
	driftedType := insightsv1.InsightType(999)
	_, err := s.Query(ctx, connect.NewRequest(&insightsv1.QueryRequest{
		InsightType: &driftedType,
	}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if code := connect.CodeOf(err); code != connect.CodeInternal {
		t.Errorf("got code %v, want CodeInternal", code)
	}
}
