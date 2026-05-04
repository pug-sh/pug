package dashboards

import (
	"testing"
	"time"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
)

func TestCreateDashboardInsightRequestRejectsDuplicateBreakpoints(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceCreateInsightRequest{
		DashboardId: proto.String("dash_123"),
		DisplayName: proto.String("Signups"),
		Query:       validQueryRequest(),
		Layouts: []*dashboardsv1.ResponsiveGridLayout{
			{Breakpoint: proto.String("lg"), X: proto.Int32(0), Y: proto.Int32(0), W: proto.Int32(6), H: proto.Int32(4)},
			{Breakpoint: proto.String("lg"), X: proto.Int32(6), Y: proto.Int32(0), W: proto.Int32(6), H: proto.Int32(4)},
		},
	}

	if err := protovalidate.Validate(req); err == nil {
		t.Fatal("expected validation error for duplicate breakpoints")
	}
}

func TestCreateDashboardInsightRequestAcceptsResponsiveLayouts(t *testing.T) {
	req := &dashboardsv1.DashboardsServiceCreateInsightRequest{
		DashboardId: proto.String("dash_123"),
		DisplayName: proto.String("Signups"),
		Query:       validQueryRequest(),
		Layouts: []*dashboardsv1.ResponsiveGridLayout{
			{Breakpoint: proto.String("lg"), X: proto.Int32(0), Y: proto.Int32(0), W: proto.Int32(6), H: proto.Int32(4), MinW: proto.Int32(3), MinH: proto.Int32(2)},
			{Breakpoint: proto.String("md"), X: proto.Int32(0), Y: proto.Int32(4), W: proto.Int32(10), H: proto.Int32(5)},
		},
	}

	if err := protovalidate.Validate(req); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func validQueryRequest() *insightsv1.QueryRequest {
	return &insightsv1.QueryRequest{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
		TimeRange: &commonv1.TimeRange{
			From: timestamppb.New(time.Now().Add(-24 * time.Hour)),
			To:   timestamppb.New(time.Now()),
		},
		Events: []*insightsv1.EventQuery{
			{
				Event: &commonv1.EventFilter{
					Kind: proto.String("signup"),
				},
			},
		},
	}
}
