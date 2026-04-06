package insights

import (
	"context"
	"testing"

	"connectrpc.com/authn"
	"connectrpc.com/connect"

	"github.com/fivebitsio/cotton/internal/app/server/rpc"
	"github.com/fivebitsio/cotton/internal/gen/proto/shared/insights/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbread"
)

func TestQuery_Unauthenticated(t *testing.T) {
	s := NewServer(nil, nil)
	_, err := s.Query(context.Background(), connect.NewRequest(&insightsv1.QueryRequest{}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if code := connect.CodeOf(err); code != connect.CodeUnauthenticated {
		t.Errorf("got code %v, want CodeUnauthenticated", code)
	}
}

func TestQuery_UnsupportedInsightType(t *testing.T) {
	ctx := authn.SetInfo(context.Background(), &rpc.Principal{
		Project: &dbread.Project{ID: "proj_test"},
	})

	s := NewServer(nil, nil)
	_, err := s.Query(ctx, connect.NewRequest(&insightsv1.QueryRequest{
		InsightType: 999,
	}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if code := connect.CodeOf(err); code != connect.CodeInvalidArgument {
		t.Errorf("got code %v, want CodeInvalidArgument", code)
	}
}

func TestSegmentUsers_Unauthenticated(t *testing.T) {
	s := NewServer(nil, nil)
	_, err := s.SegmentUsers(context.Background(), connect.NewRequest(&insightsv1.SegmentUsersRequest{}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if code := connect.CodeOf(err); code != connect.CodeUnauthenticated {
		t.Errorf("got code %v, want CodeUnauthenticated", code)
	}
}

func TestGetFilterSchema_Unauthenticated(t *testing.T) {
	s := NewServer(nil, nil)
	_, err := s.GetFilterSchema(context.Background(), connect.NewRequest(&insightsv1.GetFilterSchemaRequest{}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if code := connect.CodeOf(err); code != connect.CodeUnauthenticated {
		t.Errorf("got code %v, want CodeUnauthenticated", code)
	}
}

func TestGetPropertyValues_Unauthenticated(t *testing.T) {
	s := NewServer(nil, nil)
	_, err := s.GetPropertyValues(context.Background(), connect.NewRequest(&insightsv1.GetPropertyValuesRequest{}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if code := connect.CodeOf(err); code != connect.CodeUnauthenticated {
		t.Errorf("got code %v, want CodeUnauthenticated", code)
	}
}
