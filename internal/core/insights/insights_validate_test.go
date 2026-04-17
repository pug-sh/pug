package insights_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/fivebitsio/cotton/internal/gen/proto/common/v1"
	insightsv1 "github.com/fivebitsio/cotton/internal/gen/proto/shared/insights/v1"
)

// hasRule returns true if err is a *protovalidate.ValidationError and any of
// its violations has a rule id containing the given substring. Falls back to
// substring match against err.Error() for CEL runtime errors (e.g. failed type
// coercion), which protovalidate wraps as plain errors but include the rule id
// in the message text.
func hasRule(err error, ruleSubstring string) bool {
	var ve *protovalidate.ValidationError
	if errors.As(err, &ve) {
		for _, v := range ve.Violations {
			if strings.Contains(v.Proto.GetRuleId(), ruleSubstring) {
				return true
			}
		}
		return false
	}
	return strings.Contains(err.Error(), ruleSubstring)
}

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

// TestQueryRequest_FunnelRetentionRequireEvents exercises the
// query_request.funnel_retention_require_events CEL rule: funnel and retention
// insight types require at least one event; trends and segmentation do not.
func TestQueryRequest_FunnelRetentionRequireEvents(t *testing.T) {
	tests := []struct {
		name        string
		insightType insightsv1.InsightType
		events      []*insightsv1.EventQuery
		wantErr     bool
	}{
		{name: "funnel_zero_events_rejected", insightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL, events: nil, wantErr: true},
		{name: "retention_zero_events_rejected", insightType: insightsv1.InsightType_INSIGHT_TYPE_RETENTION, events: nil, wantErr: true},
		{name: "trends_zero_events_accepted", insightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS, events: nil, wantErr: false},
		{name: "funnel_one_event_accepted", insightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL, events: []*insightsv1.EventQuery{{Event: &commonv1.EventFilter{Kind: proto.String("signup")}}}, wantErr: false},
		{name: "retention_one_event_accepted", insightType: insightsv1.InsightType_INSIGHT_TYPE_RETENTION, events: []*insightsv1.EventQuery{{Event: &commonv1.EventFilter{Kind: proto.String("signup")}}}, wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := validQueryRequest()
			req.InsightType = tt.insightType.Enum()
			req.Events = tt.events
			err := protovalidate.Validate(req)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected validation error, got nil")
				}
				if !hasRule(err, "funnel_retention_require_events") {
					t.Errorf("expected rule funnel_retention_require_events in violations, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Errorf("expected valid, got error: %v", err)
			}
		})
	}
}

// TestEventQuery_PropertyRequiredForNumericAgg exercises the
// event_query.property_required_for_numeric_agg CEL rule: SUM/AVG/MIN/MAX
// require a non-empty aggregation_property; TOTAL/UNIQUE_USERS/PER_USER_AVG do not.
func TestEventQuery_PropertyRequiredForNumericAgg(t *testing.T) {
	tests := []struct {
		name        string
		aggregation insightsv1.AggregationType
		property    string
		wantErr     bool
	}{
		{name: "SUM_empty_property_rejected", aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_SUM, property: "", wantErr: true},
		{name: "AVG_empty_property_rejected", aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_AVG, property: "", wantErr: true},
		{name: "MIN_empty_property_rejected", aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_MIN, property: "", wantErr: true},
		{name: "MAX_empty_property_rejected", aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_MAX, property: "", wantErr: true},
		{name: "SUM_with_property_accepted", aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_SUM, property: "amount", wantErr: false},
		{name: "AVG_with_property_accepted", aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_AVG, property: "amount", wantErr: false},
		{name: "TOTAL_empty_property_accepted", aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL, property: "", wantErr: false},
		{name: "UNIQUE_USERS_empty_property_accepted", aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS, property: "", wantErr: false},
		{name: "PER_USER_AVG_empty_property_accepted", aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_PER_USER_AVG, property: "", wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := validQueryRequest()
			req.Events = []*insightsv1.EventQuery{{
				Event:               &commonv1.EventFilter{Kind: proto.String("purchase")},
				Aggregation:         tt.aggregation.Enum(),
				AggregationProperty: proto.String(tt.property),
			}}
			err := protovalidate.Validate(req)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected validation error, got nil")
				}
				if !hasRule(err, "property_required_for_numeric_agg") {
					t.Errorf("expected rule property_required_for_numeric_agg in violations, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Errorf("expected valid, got error: %v", err)
			}
		})
	}
}
