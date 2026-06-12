package dashboards

import (
	"testing"
	"time"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	publicdashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/public/dashboards/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
)

// These pin the protovalidate constraints on the unauthenticated request — the
// input-validation boundary for a public endpoint, where a CEL typo fails open.

func TestSharedQueryRequest_RequiresShareID(t *testing.T) {
	if err := protovalidate.Validate(&publicdashboardsv1.SharedDashboardsServiceQueryRequest{}); err == nil {
		t.Fatal("expected validation error for missing share_id")
	}
}

func TestSharedQueryRequest_AcceptsShareIDOnly(t *testing.T) {
	// No time_range → the `!has(this.time_range)` short-circuit must pass.
	req := &publicdashboardsv1.SharedDashboardsServiceQueryRequest{ShareId: proto.String("tok")}
	if err := protovalidate.Validate(req); err != nil {
		t.Fatalf("share_id only should pass: %v", err)
	}
}

func TestSharedQueryRequest_TimeRangeOrdering(t *testing.T) {
	mk := func(fromSec, toSec int64) *publicdashboardsv1.SharedDashboardsServiceQueryRequest {
		return &publicdashboardsv1.SharedDashboardsServiceQueryRequest{
			ShareId: proto.String("tok"),
			TimeRange: &commonv1.TimeRange{
				From: timestamppb.New(time.Unix(fromSec, 0)),
				To:   timestamppb.New(time.Unix(toSec, 0)),
			},
		}
	}
	if err := protovalidate.Validate(mk(100, 200)); err != nil {
		t.Errorf("from < to should pass: %v", err)
	}
	if err := protovalidate.Validate(mk(200, 200)); err == nil {
		t.Error("from == to should fail (from < to required)")
	}
	if err := protovalidate.Validate(mk(300, 200)); err == nil {
		t.Error("from > to should fail")
	}
}

func TestSharedQueryRequest_RejectsUndefinedGranularity(t *testing.T) {
	req := &publicdashboardsv1.SharedDashboardsServiceQueryRequest{
		ShareId:     proto.String("tok"),
		Granularity: insightsv1.Granularity(9999).Enum(),
	}
	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for undefined granularity (enum.defined_only)")
	}
}
