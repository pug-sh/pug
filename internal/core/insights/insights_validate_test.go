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
// its violations has a rule id containing the given substring. Returns false
// for any other error type — strict matching prevents accidental false-positive
// matches against CEL compile-time diagnostics that may embed rule ids verbatim.
func hasRule(err error, ruleSubstring string) bool {
	var ve *protovalidate.ValidationError
	if !errors.As(err, &ve) {
		return false
	}
	for _, v := range ve.Violations {
		if strings.Contains(v.Proto.GetRuleId(), ruleSubstring) {
			return true
		}
	}
	return false
}

// validQueryAnchor is the fixed reference time used by validQueryRequest to keep
// time-range tests independent of wall-clock drift.
var validQueryAnchor = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// validQueryRequest returns a QueryRequest with all required fields populated.
// The TimeRange is anchored to a fixed time so tests that don't override it
// remain deterministic across runs.
func validQueryRequest() *insightsv1.QueryRequest {
	return &insightsv1.QueryRequest{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
		TimeRange: &commonv1.TimeRange{
			From: timestamppb.New(validQueryAnchor),
			To:   timestamppb.New(validQueryAnchor.Add(24 * time.Hour)),
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

// TestQueryRequest_GranularityMaxRange exercises the five
// query_request.granularity_{minute,hour,day,week,month}_max_range CEL rules.
// A fixed anchor time is used so boundary cases align exactly with the
// duration literals in CEL.
func TestQueryRequest_GranularityMaxRange(t *testing.T) {
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rng := func(d time.Duration) *commonv1.TimeRange {
		return &commonv1.TimeRange{
			From: timestamppb.New(anchor),
			To:   timestamppb.New(anchor.Add(d)),
		}
	}
	const (
		hour = time.Hour
		day  = 24 * time.Hour
	)
	tests := []struct {
		name        string
		granularity insightsv1.Granularity
		timeRange   *commonv1.TimeRange
		wantRule    string // empty means expect no error
	}{
		{name: "minute_within_6h", granularity: insightsv1.Granularity_GRANULARITY_MINUTE, timeRange: rng(5*hour + 59*time.Minute)},
		{name: "minute_just_under_6h", granularity: insightsv1.Granularity_GRANULARITY_MINUTE, timeRange: rng(6*hour - time.Nanosecond)},
		{name: "minute_at_6h", granularity: insightsv1.Granularity_GRANULARITY_MINUTE, timeRange: rng(6 * hour)},
		{name: "minute_over_6h", granularity: insightsv1.Granularity_GRANULARITY_MINUTE, timeRange: rng(6*hour + time.Minute), wantRule: "granularity_minute_max_range"},

		{name: "hour_within_14d", granularity: insightsv1.Granularity_GRANULARITY_HOUR, timeRange: rng(13 * day)},
		{name: "hour_just_under_336h", granularity: insightsv1.Granularity_GRANULARITY_HOUR, timeRange: rng(336*hour - time.Nanosecond)},
		{name: "hour_at_336h", granularity: insightsv1.Granularity_GRANULARITY_HOUR, timeRange: rng(336 * hour)},
		{name: "hour_over_14d", granularity: insightsv1.Granularity_GRANULARITY_HOUR, timeRange: rng(14*day + time.Minute), wantRule: "granularity_hour_max_range"},

		{name: "day_within_365d", granularity: insightsv1.Granularity_GRANULARITY_DAY, timeRange: rng(364 * day)},
		{name: "day_just_under_8760h", granularity: insightsv1.Granularity_GRANULARITY_DAY, timeRange: rng(8760*hour - time.Nanosecond)},
		{name: "day_at_8760h", granularity: insightsv1.Granularity_GRANULARITY_DAY, timeRange: rng(8760 * hour)},
		{name: "day_over_365d", granularity: insightsv1.Granularity_GRANULARITY_DAY, timeRange: rng(366 * day), wantRule: "granularity_day_max_range"},

		{name: "week_within_4y", granularity: insightsv1.Granularity_GRANULARITY_WEEK, timeRange: rng(3 * 365 * day)},
		{name: "week_just_under_35064h", granularity: insightsv1.Granularity_GRANULARITY_WEEK, timeRange: rng(35064*hour - time.Nanosecond)},
		{name: "week_at_35064h", granularity: insightsv1.Granularity_GRANULARITY_WEEK, timeRange: rng(35064 * hour)},
		{name: "week_over_4y", granularity: insightsv1.Granularity_GRANULARITY_WEEK, timeRange: rng(35064*hour + time.Minute), wantRule: "granularity_week_max_range"},

		{name: "month_within_10y", granularity: insightsv1.Granularity_GRANULARITY_MONTH, timeRange: rng(9 * 365 * day)},
		{name: "month_just_under_87660h", granularity: insightsv1.Granularity_GRANULARITY_MONTH, timeRange: rng(87660*hour - time.Nanosecond)},
		{name: "month_at_87660h", granularity: insightsv1.Granularity_GRANULARITY_MONTH, timeRange: rng(87660 * hour)},
		{name: "month_over_10y", granularity: insightsv1.Granularity_GRANULARITY_MONTH, timeRange: rng(87660*hour + time.Minute), wantRule: "granularity_month_max_range"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := validQueryRequest()
			req.Granularity = tt.granularity.Enum()
			req.TimeRange = tt.timeRange
			err := protovalidate.Validate(req)
			if tt.wantRule == "" {
				if err != nil {
					t.Errorf("expected valid, got error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected rule %s violation, got nil", tt.wantRule)
			}
			if !hasRule(err, tt.wantRule) {
				t.Errorf("expected rule %s in violations, got: %v", tt.wantRule, err)
			}
		})
	}
}

// TestQueryRequest_GranularityMaxRange_EdgeRanges pins cross-rule ordering and
// degenerate-range handling: zero range and negative range are caught by
// from_before_to on TimeRange (not the granularity cap, regardless of which
// granularity is chosen), and nil time_range is caught by the field-level
// required guard before message-level CEL can dereference a nil submessage.
func TestQueryRequest_GranularityMaxRange_EdgeRanges(t *testing.T) {
	now := timestamppb.Now()
	zeroRange := &commonv1.TimeRange{From: now, To: now}
	negativeRange := &commonv1.TimeRange{
		From: timestamppb.New(now.AsTime().Add(time.Hour)),
		To:   timestamppb.New(now.AsTime()),
	}

	tests := []struct {
		name        string
		granularity insightsv1.Granularity
		timeRange   *commonv1.TimeRange
	}{
		{name: "zero_range_minute", granularity: insightsv1.Granularity_GRANULARITY_MINUTE, timeRange: zeroRange},
		{name: "zero_range_month", granularity: insightsv1.Granularity_GRANULARITY_MONTH, timeRange: zeroRange},
		{name: "negative_range_minute", granularity: insightsv1.Granularity_GRANULARITY_MINUTE, timeRange: negativeRange},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := validQueryRequest()
			req.Granularity = tt.granularity.Enum()
			req.TimeRange = tt.timeRange
			err := protovalidate.Validate(req)
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !hasRule(err, "from_before_to") {
				t.Errorf("expected from_before_to violation, got: %v", err)
			}
		})
	}

	t.Run("nil_time_range_rejected_by_required", func(t *testing.T) {
		req := validQueryRequest()
		req.Granularity = insightsv1.Granularity_GRANULARITY_MINUTE.Enum()
		req.TimeRange = nil
		err := protovalidate.Validate(req)
		if err == nil {
			t.Fatal("expected validation error, got nil")
		}
		if !strings.Contains(err.Error(), "time_range") {
			t.Errorf("expected time_range-related violation, got: %v", err)
		}
	})

	// over_cap_AND_negative_range pins which rule fires when from>to AND the absolute span
	// would also exceed the granularity cap. Because CEL evaluates `to - from` as a negative
	// duration (which is trivially <= duration('6h')), only from_before_to fires; the granularity
	// cap rule short-circuits. Pinning both halves protects against future CEL changes.
	t.Run("over_cap_AND_negative_range_reports_only_from_before_to", func(t *testing.T) {
		req := validQueryRequest()
		req.Granularity = insightsv1.Granularity_GRANULARITY_MINUTE.Enum()
		req.TimeRange = &commonv1.TimeRange{
			From: timestamppb.New(validQueryAnchor.Add(7 * time.Hour)),
			To:   timestamppb.New(validQueryAnchor),
		}
		err := protovalidate.Validate(req)
		if !hasRule(err, "from_before_to") {
			t.Errorf("expected from_before_to violation, got: %v", err)
		}
		if hasRule(err, "granularity_minute_max_range") {
			t.Errorf("did not expect granularity_minute_max_range to fire on negative duration, got: %v", err)
		}
	})
}

// TestQueryRequest_GranularityMaxRange_AllInsightTypes pins the CLAUDE.md claim that
// the granularity caps fire regardless of insight_type, even though funnel and
// segmentation builders ignore granularity at query-build time.
func TestQueryRequest_GranularityMaxRange_AllInsightTypes(t *testing.T) {
	twoEvents := []*insightsv1.EventQuery{
		{Event: &commonv1.EventFilter{Kind: proto.String("step_a")}},
		{Event: &commonv1.EventFilter{Kind: proto.String("step_b")}},
	}
	overCap := &commonv1.TimeRange{
		From: timestamppb.New(validQueryAnchor),
		To:   timestamppb.New(validQueryAnchor.Add(7 * time.Hour)), // > 6h MINUTE cap
	}

	tests := []struct {
		name        string
		insightType insightsv1.InsightType
		events      []*insightsv1.EventQuery
	}{
		{name: "trends", insightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS, events: twoEvents},
		{name: "segmentation", insightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION, events: twoEvents},
		{name: "funnel", insightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL, events: twoEvents},
		{name: "retention", insightType: insightsv1.InsightType_INSIGHT_TYPE_RETENTION, events: twoEvents},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := validQueryRequest()
			req.InsightType = tt.insightType.Enum()
			req.Events = tt.events
			req.Granularity = insightsv1.Granularity_GRANULARITY_MINUTE.Enum()
			req.TimeRange = overCap
			err := protovalidate.Validate(req)
			if !hasRule(err, "granularity_minute_max_range") {
				t.Errorf("expected granularity_minute_max_range to fire for %s, got: %v", tt.name, err)
			}
		})
	}
}

// TestGranularityValidation exercises the required + defined_only + not_in:[0]
// constraints on Granularity. UNSPECIFIED is rejected at the field level.
// Uses a 1-hour time range that satisfies every granularity's max-range cap so
// the field-level validation is exercised in isolation from the message-level caps.
func TestGranularityValidation(t *testing.T) {
	smallRange := &commonv1.TimeRange{
		From: timestamppb.New(validQueryAnchor),
		To:   timestamppb.New(validQueryAnchor.Add(time.Hour)),
	}
	tests := []struct {
		name        string
		granularity insightsv1.Granularity
		wantErr     bool
		wantField   string // when wantErr, the violation should reference this field name
	}{
		{name: "valid_minute", granularity: insightsv1.Granularity_GRANULARITY_MINUTE, wantErr: false},
		{name: "valid_hour", granularity: insightsv1.Granularity_GRANULARITY_HOUR, wantErr: false},
		{name: "valid_day", granularity: insightsv1.Granularity_GRANULARITY_DAY, wantErr: false},
		{name: "valid_week", granularity: insightsv1.Granularity_GRANULARITY_WEEK, wantErr: false},
		{name: "valid_month", granularity: insightsv1.Granularity_GRANULARITY_MONTH, wantErr: false},
		{name: "unspecified_rejected", granularity: insightsv1.Granularity_GRANULARITY_UNSPECIFIED, wantErr: true, wantField: "granularity"},
		{name: "undefined_value_rejected", granularity: 999, wantErr: true, wantField: "granularity"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := validQueryRequest()
			req.Granularity = tt.granularity.Enum()
			req.TimeRange = smallRange
			err := protovalidate.Validate(req)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected validation error, got nil")
				}
				if tt.wantField != "" && !strings.Contains(err.Error(), tt.wantField) {
					t.Errorf("expected violation referencing field %q, got: %v", tt.wantField, err)
				}
				return
			}
			if err != nil {
				t.Errorf("expected valid, got error: %v", err)
			}
		})
	}
}
