package insights_test

import (
	"testing"

	"time"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/fivebitsio/cotton/internal/gen/proto/common/v1"
	insightsv1 "github.com/fivebitsio/cotton/internal/gen/proto/shared/insights/v1"
)

// validQueryRequest returns a QueryRequest with all required fields populated.
func validQueryRequest() *insightsv1.QueryRequest {
	return &insightsv1.QueryRequest{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
		TimeRange: &commonv1.TimeRange{
			From: timestamppb.New(time.Now().Add(-24 * time.Hour)),
			To:   timestamppb.Now(),
		},
		Events: []*insightsv1.EventQuery{{
			Event: &commonv1.EventFilter{Kind: proto.String("page_view")},
		}},
	}
}

// TestInsightTypeValidation exercises the required + defined_only constraints on InsightType.
func TestInsightTypeValidation(t *testing.T) {
	tests := []struct {
		name        string
		insightType insightsv1.InsightType
		wantErr     bool
	}{
		{name: "valid_trends", insightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS, wantErr: false},
		{name: "valid_funnel", insightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL, wantErr: false},
		{name: "valid_retention", insightType: insightsv1.InsightType_INSIGHT_TYPE_RETENTION, wantErr: false},
		{name: "valid_segmentation", insightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION, wantErr: false},
		{name: "unspecified_rejected", insightType: insightsv1.InsightType_INSIGHT_TYPE_UNSPECIFIED, wantErr: true},
		{name: "undefined_value_rejected", insightType: 999, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := validQueryRequest()
			req.InsightType = tt.insightType.Enum()
			err := protovalidate.Validate(req)
			if tt.wantErr && err == nil {
				t.Error("expected validation error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("expected valid, got error: %v", err)
			}
		})
	}
}
