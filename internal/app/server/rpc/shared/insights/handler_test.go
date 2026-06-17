package insights

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/apperr"
	coreinsights "github.com/pug-sh/pug/internal/core/insights"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
)

func TestQuery_Unauthenticated(t *testing.T) {
	s := &server{}
	_, err := s.Query(context.Background(), connect.NewRequest(&insightsv1.QueryRequest{}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code() != connect.CodeUnauthenticated {
		t.Fatalf("want unauthenticated apperr, got %v (%T)", err, err)
	}
}

func TestSegmentUsers_Unauthenticated(t *testing.T) {
	s := &server{}
	_, err := s.SegmentUsers(context.Background(), connect.NewRequest(&insightsv1.SegmentUsersRequest{}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code() != connect.CodeUnauthenticated {
		t.Fatalf("want unauthenticated apperr, got %v (%T)", err, err)
	}
}

func TestGetFilterSchema_Unauthenticated(t *testing.T) {
	s := &server{}
	_, err := s.GetFilterSchema(context.Background(), connect.NewRequest(&commonv1.GetFilterSchemaRequest{}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code() != connect.CodeUnauthenticated {
		t.Fatalf("want unauthenticated apperr, got %v (%T)", err, err)
	}
}

func TestGetPropertyValues_Unauthenticated(t *testing.T) {
	s := &server{}
	_, err := s.GetPropertyValues(context.Background(), connect.NewRequest(&insightsv1.GetPropertyValuesRequest{}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code() != connect.CodeUnauthenticated {
		t.Fatalf("want unauthenticated apperr, got %v (%T)", err, err)
	}
}

// The bucketing timezone is server-authoritative: the handler must overwrite any
// client-supplied QueryRequest.timezone with the project's stored reporting_timezone
// before executing. We assert on the mutated request rather than the downstream
// result (the empty Executor fails execution, but the overwrite happens first).
func TestQuery_InjectsProjectTimezone_IgnoresClient(t *testing.T) {
	ctx := authn.SetInfo(context.Background(), &rpc.Principal{
		Project: &dbread.Project{ID: "test-project", ReportingTimezone: "Asia/Kolkata"},
	})
	s := &server{executor: &coreinsights.Executor{}}
	insightType := insightsv1.InsightType_INSIGHT_TYPE_TRENDS
	req := connect.NewRequest(&insightsv1.QueryRequest{
		Spec:     &insightsv1.InsightQuerySpec{InsightType: &insightType},
		Timezone: proto.String("America/New_York"), // client-supplied; must be ignored
	})
	_, _ = s.Query(ctx, req) // fails downstream (no ClickHouse); we only check the override
	if got := req.Msg.GetTimezone(); got != "Asia/Kolkata" {
		t.Errorf("timezone = %q, want project's Asia/Kolkata (client value must be ignored)", got)
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
	s := &server{executor: &coreinsights.Executor{}}
	driftedType := insightsv1.InsightType(999)
	_, err := s.Query(ctx, connect.NewRequest(&insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: &driftedType,
		},
	}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if code := connect.CodeOf(err); code != connect.CodeInternal {
		t.Errorf("got code %v, want CodeInternal", code)
	}
}

// TestQuery_InvalidBuildError verifies that a Build*Query failure returns CodeInvalidArgument
// tagged with ReasonInvalidInsightQuery. A trends request with a filter group that has no
// filters bypasses protovalidate (this test calls the handler directly) and triggers the
// builder error path, exercising the apperr.Invalid conversion for all Build* sites.
func TestQuery_InvalidBuildError(t *testing.T) {
	ctx := authn.SetInfo(context.Background(), &rpc.Principal{
		Project: &dbread.Project{ID: "test-project"},
	})
	s := &server{executor: &coreinsights.Executor{}}
	insightType := insightsv1.InsightType_INSIGHT_TYPE_TRENDS
	_, err := s.Query(ctx, connect.NewRequest(&insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: &insightType,
			// A filter group with no filters triggers "group must contain at least one filter"
			// inside buildSingleFilterGroupCondition — this exercises the slog.WarnContext +
			// apperr.Invalid(ReasonInvalidInsightQuery, ...) path.
			FilterGroups: []*insightsv1.FilterGroup{{}},
		},
	}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var ae *apperr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("want apperr.Error, got %T: %v", err, err)
	}
	if ae.Code() != connect.CodeInvalidArgument {
		t.Errorf("want CodeInvalidArgument, got %v", ae.Code())
	}
	if ae.Reason() != apperr.ReasonInvalidInsightQuery {
		t.Errorf("want reason %q, got %q", apperr.ReasonInvalidInsightQuery, ae.Reason())
	}
}
