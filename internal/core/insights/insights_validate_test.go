package insights_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
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
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
			Events: []*insightsv1.EventQuery{{
				Event: &commonv1.EventFilter{Kind: proto.String("page_view")},
			}},
		},
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
		TimeRange: &commonv1.TimeRange{
			From: timestamppb.New(validQueryAnchor),
			To:   timestamppb.New(validQueryAnchor.Add(24 * time.Hour)),
		},
	}
}

func validUserFlowQueryRequest() *insightsv1.QueryRequest {
	return &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_USER_FLOW.Enum(),
			UserFlow:    &insightsv1.UserFlowQuery{},
		},
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
		TimeRange: &commonv1.TimeRange{
			From: timestamppb.New(validQueryAnchor),
			To:   timestamppb.New(validQueryAnchor.Add(24 * time.Hour)),
		},
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
		{name: "valid_user_flow", insightType: insightsv1.InsightType_INSIGHT_TYPE_USER_FLOW, wantErr: false},
		{name: "unspecified_rejected", insightType: insightsv1.InsightType_INSIGHT_TYPE_UNSPECIFIED, wantErr: true},
		{name: "undefined_value_rejected", insightType: 999, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := validQueryRequest()
			if tt.insightType == insightsv1.InsightType_INSIGHT_TYPE_USER_FLOW {
				req = validUserFlowQueryRequest()
			}
			req.Spec.InsightType = tt.insightType.Enum()
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
		{name: "user_flow_zero_events_accepted", insightType: insightsv1.InsightType_INSIGHT_TYPE_USER_FLOW, events: nil, wantErr: false},
		{name: "funnel_one_event_accepted", insightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL, events: []*insightsv1.EventQuery{{Event: &commonv1.EventFilter{Kind: proto.String("signup")}}}, wantErr: false},
		{name: "retention_one_event_accepted", insightType: insightsv1.InsightType_INSIGHT_TYPE_RETENTION, events: []*insightsv1.EventQuery{{Event: &commonv1.EventFilter{Kind: proto.String("signup")}}}, wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := validQueryRequest()
			if tt.insightType == insightsv1.InsightType_INSIGHT_TYPE_USER_FLOW {
				req = validUserFlowQueryRequest()
			}
			req.Spec.InsightType = tt.insightType.Enum()
			req.Spec.Events = tt.events
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

// TestUserFlowQueryValidation exercises user-flow-specific CEL rules on InsightQuerySpec.
func TestUserFlowQueryValidation(t *testing.T) {
	t.Run("property_requires_node_property", func(t *testing.T) {
		req := validUserFlowQueryRequest()
		req.Spec.UserFlow = &insightsv1.UserFlowQuery{
			NodeKind: insightsv1.UserFlowQuery_NODE_KIND_PROPERTY.Enum(),
		}
		err := protovalidate.Validate(req)
		if err == nil {
			t.Fatal("expected validation error, got nil")
		}
		if !hasRule(err, "user_flow_property_required") {
			t.Errorf("expected rule user_flow_property_required, got: %v", err)
		}
	})

	t.Run("empty_user_flow_valid", func(t *testing.T) {
		if err := protovalidate.Validate(validUserFlowQueryRequest()); err != nil {
			t.Fatalf("expected valid, got error: %v", err)
		}
	})

	t.Run("user_flow_rejects_events", func(t *testing.T) {
		req := validUserFlowQueryRequest()
		req.Spec.Events = []*insightsv1.EventQuery{{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}}}
		err := protovalidate.Validate(req)
		if err == nil {
			t.Fatal("expected validation error, got nil")
		}
		if !hasRule(err, "user_flow_no_events") {
			t.Errorf("expected rule user_flow_no_events, got: %v", err)
		}
	})

	t.Run("user_flow_required_when_insight_type_is_user_flow", func(t *testing.T) {
		req := validUserFlowQueryRequest()
		req.Spec.UserFlow = nil
		if err := protovalidate.Validate(req); !hasRule(err, "user_flow_required") {
			t.Errorf("expected user_flow_required, got: %v", err)
		}
	})

	t.Run("user_flow_only_for_user_flow_insight_type", func(t *testing.T) {
		req := validQueryRequest() // TRENDS with events — otherwise valid
		req.Spec.UserFlow = &insightsv1.UserFlowQuery{}
		if err := protovalidate.Validate(req); !hasRule(err, "user_flow_only_for_user_flow") {
			t.Errorf("expected user_flow_only_for_user_flow, got: %v", err)
		}
	})

	t.Run("user_flow_rejects_session", func(t *testing.T) {
		req := validUserFlowQueryRequest()
		req.Spec.Session = &insightsv1.SessionQuery{
			Metric: insightsv1.SessionMetric_SESSION_METRIC_SESSIONS.Enum(),
		}
		if err := protovalidate.Validate(req); !hasRule(err, "user_flow_no_session") {
			t.Errorf("expected user_flow_no_session, got: %v", err)
		}
	})

	t.Run("user_flow_rejects_breakdowns", func(t *testing.T) {
		req := validUserFlowQueryRequest()
		req.Spec.Breakdowns = []*insightsv1.Breakdown{{Property: proto.String("$url")}}
		if err := protovalidate.Validate(req); !hasRule(err, "user_flow_no_breakdowns") {
			t.Errorf("expected user_flow_no_breakdowns, got: %v", err)
		}
	})

	t.Run("user_flow_rejects_breakdown_limit", func(t *testing.T) {
		req := validUserFlowQueryRequest()
		req.Spec.BreakdownLimit = proto.Int32(5)
		if err := protovalidate.Validate(req); !hasRule(err, "user_flow_no_breakdown_limit") {
			t.Errorf("expected user_flow_no_breakdown_limit, got: %v", err)
		}
	})

	t.Run("node_property_pattern_rejects_invalid_chars", func(t *testing.T) {
		req := validUserFlowQueryRequest()
		req.Spec.UserFlow = &insightsv1.UserFlowQuery{
			NodeKind:     insightsv1.UserFlowQuery_NODE_KIND_PROPERTY.Enum(),
			NodeProperty: proto.String("bad prop"), // space is outside the field pattern
		}
		if err := protovalidate.Validate(req); err == nil {
			t.Error("expected validation error for node_property containing a space")
		}
	})

	// Range rules: 0 means unset (valid); the documented boundaries are valid;
	// just-outside values are rejected by their dedicated rule.
	rangeTests := []struct {
		name string
		mut  func(*insightsv1.UserFlowQuery)
		rule string // "" => expect the request to be valid
	}{
		{"max_hops_unset_ok", func(q *insightsv1.UserFlowQuery) { q.MaxHops = proto.Int32(0) }, ""},
		{"max_hops_min_ok", func(q *insightsv1.UserFlowQuery) { q.MaxHops = proto.Int32(1) }, ""},
		{"max_hops_max_ok", func(q *insightsv1.UserFlowQuery) { q.MaxHops = proto.Int32(10) }, ""},
		{"max_hops_over_max", func(q *insightsv1.UserFlowQuery) { q.MaxHops = proto.Int32(11) }, "user_flow_max_hops_range"},
		{"max_nodes_under_min", func(q *insightsv1.UserFlowQuery) { q.MaxNodes = proto.Int32(1) }, "user_flow_max_nodes_range"},
		{"max_nodes_min_ok", func(q *insightsv1.UserFlowQuery) { q.MaxNodes = proto.Int32(2) }, ""},
		{"max_nodes_max_ok", func(q *insightsv1.UserFlowQuery) { q.MaxNodes = proto.Int32(50) }, ""},
		{"max_nodes_over_max", func(q *insightsv1.UserFlowQuery) { q.MaxNodes = proto.Int32(51) }, "user_flow_max_nodes_range"},
		{"max_links_min_ok", func(q *insightsv1.UserFlowQuery) { q.MaxLinks = proto.Int32(1) }, ""},
		{"max_links_max_ok", func(q *insightsv1.UserFlowQuery) { q.MaxLinks = proto.Int32(500) }, ""},
		{"max_links_over_max", func(q *insightsv1.UserFlowQuery) { q.MaxLinks = proto.Int32(501) }, "user_flow_max_links_range"},
	}
	for _, tt := range rangeTests {
		t.Run(tt.name, func(t *testing.T) {
			req := validUserFlowQueryRequest()
			tt.mut(req.Spec.UserFlow)
			err := protovalidate.Validate(req)
			if tt.rule == "" {
				if err != nil {
					t.Errorf("expected valid, got: %v", err)
				}
				return
			}
			if !hasRule(err, tt.rule) {
				t.Errorf("expected rule %s, got: %v", tt.rule, err)
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
			req.Spec.Events = []*insightsv1.EventQuery{{
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
			req.Spec.InsightType = tt.insightType.Enum()
			req.Spec.Events = tt.events
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

// TestFunnelOnlyConversionWindow exercises the funnel_only_conversion_window CEL rule.
// The rule changed semantics this PR — from "conversion_window_seconds == 0" to
// "!has(this.conversion_window)" — so any *set* (even zero-Duration) field on a non-funnel
// insight type now fails validation.
func TestFunnelOnlyConversionWindow(t *testing.T) {
	tests := []struct {
		name        string
		insightType insightsv1.InsightType
		window      *durationpb.Duration
		wantErr     bool
		wantRule    string
	}{
		{
			name:        "funnel with window — accepted",
			insightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL,
			window:      durationpb.New(1 * time.Hour),
		},
		{
			name:        "funnel without window — accepted",
			insightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL,
			window:      nil,
		},
		{
			name:        "trends without window — accepted",
			insightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS,
			window:      nil,
		},
		{
			name:        "trends with explicit window — rejected",
			insightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS,
			window:      durationpb.New(1 * time.Hour),
			wantErr:     true,
			wantRule:    "funnel_only_conversion_window",
		},
		{
			name:        "retention with window — rejected",
			insightType: insightsv1.InsightType_INSIGHT_TYPE_RETENTION,
			window:      durationpb.New(30 * time.Minute),
			wantErr:     true,
			wantRule:    "funnel_only_conversion_window",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := validQueryRequest()
			req.Spec.InsightType = tt.insightType.Enum()
			req.Spec.ConversionWindow = tt.window
			if tt.insightType == insightsv1.InsightType_INSIGHT_TYPE_FUNNEL ||
				tt.insightType == insightsv1.InsightType_INSIGHT_TYPE_RETENTION {
				req.Spec.Events = append(req.Spec.Events, &insightsv1.EventQuery{
					Event: &commonv1.EventFilter{Kind: proto.String("purchase")},
				})
			}
			err := protovalidate.Validate(req)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected validation error, got nil")
				}
				if tt.wantRule != "" && !hasRule(err, tt.wantRule) {
					t.Errorf("expected rule %q in violations, got: %v", tt.wantRule, err)
				}
				return
			}
			if err != nil {
				t.Errorf("expected valid, got error: %v", err)
			}
		})
	}
}

// TestFunnelOnlyStepTiming exercises the funnel_only_step_timing CEL rule.
// include_step_timing must be false on non-funnel requests.
func TestFunnelOnlyStepTiming(t *testing.T) {
	tests := []struct {
		name        string
		insightType insightsv1.InsightType
		include     bool
		wantErr     bool
		wantRule    string
	}{
		{
			name:        "funnel with include_step_timing=true — accepted",
			insightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL,
			include:     true,
		},
		{
			name:        "funnel with include_step_timing=false — accepted",
			insightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL,
			include:     false,
		},
		{
			name:        "trends with include_step_timing=false — accepted",
			insightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS,
			include:     false,
		},
		{
			name:        "trends with include_step_timing=true — rejected",
			insightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS,
			include:     true,
			wantErr:     true,
			wantRule:    "funnel_only_step_timing",
		},
		{
			name:        "segmentation with include_step_timing=true — rejected",
			insightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION,
			include:     true,
			wantErr:     true,
			wantRule:    "funnel_only_step_timing",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := validQueryRequest()
			req.Spec.InsightType = tt.insightType.Enum()
			req.Spec.IncludeStepTiming = proto.Bool(tt.include)
			if tt.insightType == insightsv1.InsightType_INSIGHT_TYPE_FUNNEL {
				req.Spec.Events = append(req.Spec.Events, &insightsv1.EventQuery{
					Event: &commonv1.EventFilter{Kind: proto.String("purchase")},
				})
			}
			err := protovalidate.Validate(req)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected validation error, got nil")
				}
				if tt.wantRule != "" && !hasRule(err, tt.wantRule) {
					t.Errorf("expected rule %q in violations, got: %v", tt.wantRule, err)
				}
				return
			}
			if err != nil {
				t.Errorf("expected valid, got error: %v", err)
			}
		})
	}
}

// TestConversionWindowMinimum verifies the field-level duration.gte = {seconds: 1}
// constraint: sub-second windows must be rejected at the boundary, since windowFunnel
// only accepts whole-second windows.
func TestConversionWindowMinimum(t *testing.T) {
	tests := []struct {
		name    string
		window  *durationpb.Duration
		wantErr bool
	}{
		{name: "1s — accepted", window: durationpb.New(1 * time.Second)},
		{name: "1h — accepted", window: durationpb.New(1 * time.Hour)},
		{name: "500ms — rejected (sub-second)", window: durationpb.New(500 * time.Millisecond), wantErr: true},
		{name: "0s — rejected (sub-second)", window: durationpb.New(0), wantErr: true},
		{name: "negative — rejected", window: durationpb.New(-1 * time.Second), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := validQueryRequest()
			req.Spec.InsightType = insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum()
			req.Spec.Events = append(req.Spec.Events, &insightsv1.EventQuery{
				Event: &commonv1.EventFilter{Kind: proto.String("purchase")},
			})
			req.Spec.ConversionWindow = tt.window
			err := protovalidate.Validate(req)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected validation error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("expected valid, got error: %v", err)
			}
		})
	}
}

// TestConversionWindowWholeSeconds verifies the conversion_window.whole_seconds CEL rule:
// fractional-second durations are rejected at the boundary because windowFunnel only
// accepts integer seconds (sub-second precision would silently truncate).
func TestConversionWindowWholeSeconds(t *testing.T) {
	tests := []struct {
		name    string
		window  *durationpb.Duration
		wantErr bool
	}{
		{name: "1s — accepted", window: durationpb.New(1 * time.Second)},
		{name: "30s — accepted", window: durationpb.New(30 * time.Second)},
		{name: "1h — accepted", window: durationpb.New(1 * time.Hour)},
		{name: "1500ms — rejected (sub-second precision)", window: durationpb.New(1500 * time.Millisecond), wantErr: true},
		{name: "1s + 1ns — rejected", window: durationpb.New(time.Second + time.Nanosecond), wantErr: true},
		{name: "2700ms — rejected", window: durationpb.New(2700 * time.Millisecond), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := validQueryRequest()
			req.Spec.InsightType = insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum()
			req.Spec.Events = append(req.Spec.Events, &insightsv1.EventQuery{
				Event: &commonv1.EventFilter{Kind: proto.String("purchase")},
			})
			req.Spec.ConversionWindow = tt.window
			err := protovalidate.Validate(req)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected validation error, got nil")
				}
				if !hasRule(err, "conversion_window.whole_seconds") {
					t.Errorf("expected rule conversion_window.whole_seconds in violations, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Errorf("expected valid, got error: %v", err)
			}
		})
	}
}

// TestStepTiming_DistributionExactly8Buckets exercises the StepTiming.distribution
// repeated min_items: 8 / max_items: 8 constraint. StepTiming is a response message
// (server-emitted), so this rule mainly protects against future server-side paths
// that construct StepTiming outside newStepTiming(). The rule is verified by
// constructing StepTiming directly and validating.
func TestStepTiming_DistributionExactly8Buckets(t *testing.T) {
	mkBuckets := func(n int) []*insightsv1.DistributionBucket {
		out := make([]*insightsv1.DistributionBucket, n)
		for i := range out {
			out[i] = &insightsv1.DistributionBucket{Label: proto.String("bucket"), Count: proto.Int64(0)}
		}
		return out
	}
	tests := []struct {
		name    string
		n       int
		wantErr bool
	}{
		{name: "seven_rejected", n: 7, wantErr: true},
		{name: "eight_accepted", n: 8, wantErr: false},
		{name: "nine_rejected", n: 9, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &insightsv1.StepTiming{Distribution: mkBuckets(tt.n)}
			err := protovalidate.Validate(st)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected validation error for %d-bucket distribution, got nil", tt.n)
				}
				return
			}
			if err != nil {
				t.Errorf("expected valid 8-bucket distribution, got error: %v", err)
			}
		})
	}
}

// mkSequentialEvents returns n EventQuery values with kinds step_0, step_1, ...
// Used by tests that need to exercise per-event-count CEL rules.
func mkSequentialEvents(n int) []*insightsv1.EventQuery {
	events := make([]*insightsv1.EventQuery, n)
	for i := range events {
		events[i] = &insightsv1.EventQuery{
			Event: &commonv1.EventFilter{Kind: proto.String(fmt.Sprintf("step_%d", i))},
		}
	}
	return events
}

// TestFunnelMaxSteps exercises the funnel_max_steps CEL rule:
// funnel insights cap at 20 steps; other insight types are unaffected.
func TestFunnelMaxSteps(t *testing.T) {
	tests := []struct {
		name        string
		insightType insightsv1.InsightType
		n           int
		wantErr     bool
		wantRule    string
	}{
		{name: "funnel_1_step_accepted", insightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL, n: 1},
		{name: "funnel_20_steps_accepted", insightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL, n: 20},
		{name: "funnel_21_steps_rejected", insightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL, n: 21, wantErr: true, wantRule: "funnel_max_steps"},
		// Cap fires only on funnel; trends accepts arbitrarily many events.
		{name: "trends_30_events_accepted", insightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS, n: 30},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := validQueryRequest()
			req.Spec.InsightType = tt.insightType.Enum()
			req.Spec.Events = mkSequentialEvents(tt.n)
			err := protovalidate.Validate(req)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected validation error, got nil")
				}
				if tt.wantRule != "" && !hasRule(err, tt.wantRule) {
					t.Errorf("expected rule %q in violations, got: %v", tt.wantRule, err)
				}
				return
			}
			if err != nil {
				t.Errorf("expected valid, got error: %v", err)
			}
		})
	}
}

// TestRetentionMaxEvents exercises the retention_max_events CEL rule:
// retention accepts at most 2 events (start + optional return).
func TestRetentionMaxEvents(t *testing.T) {
	tests := []struct {
		name        string
		insightType insightsv1.InsightType
		n           int
		wantErr     bool
		wantRule    string
	}{
		{name: "retention_1_event_accepted", insightType: insightsv1.InsightType_INSIGHT_TYPE_RETENTION, n: 1},
		{name: "retention_2_events_accepted", insightType: insightsv1.InsightType_INSIGHT_TYPE_RETENTION, n: 2},
		{name: "retention_3_events_rejected", insightType: insightsv1.InsightType_INSIGHT_TYPE_RETENTION, n: 3, wantErr: true, wantRule: "retention_max_events"},
		// Cap fires only on retention; trends accepts arbitrarily many events.
		{name: "trends_5_events_accepted", insightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS, n: 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := validQueryRequest()
			req.Spec.InsightType = tt.insightType.Enum()
			req.Spec.Events = mkSequentialEvents(tt.n)
			err := protovalidate.Validate(req)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected validation error, got nil")
				}
				if tt.wantRule != "" && !hasRule(err, tt.wantRule) {
					t.Errorf("expected rule %q in violations, got: %v", tt.wantRule, err)
				}
				return
			}
			if err != nil {
				t.Errorf("expected valid, got error: %v", err)
			}
		})
	}
}

// TestUniqueBreakdownProperties exercises the unique_breakdown_properties CEL rule:
// duplicate breakdown property names produce silently wrong aggregation, so reject early.
func TestUniqueBreakdownProperties(t *testing.T) {
	bd := func(prop string) *insightsv1.Breakdown {
		return &insightsv1.Breakdown{Property: proto.String(prop)}
	}
	tests := []struct {
		name       string
		breakdowns []*insightsv1.Breakdown
		wantErr    bool
		wantRule   string
	}{
		{name: "no_breakdowns_accepted", breakdowns: nil},
		{name: "single_breakdown_accepted", breakdowns: []*insightsv1.Breakdown{bd("$country")}},
		{name: "two_unique_breakdowns_accepted", breakdowns: []*insightsv1.Breakdown{bd("$country"), bd("$browser")}},
		{name: "two_duplicate_breakdowns_rejected", breakdowns: []*insightsv1.Breakdown{bd("$country"), bd("$country")}, wantErr: true, wantRule: "unique_breakdown_properties"},
		{name: "duplicate_among_three_rejected", breakdowns: []*insightsv1.Breakdown{bd("$country"), bd("$browser"), bd("$country")}, wantErr: true, wantRule: "unique_breakdown_properties"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := validQueryRequest()
			req.Spec.Breakdowns = tt.breakdowns
			err := protovalidate.Validate(req)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected validation error, got nil")
				}
				if tt.wantRule != "" && !hasRule(err, tt.wantRule) {
					t.Errorf("expected rule %q in violations, got: %v", tt.wantRule, err)
				}
				return
			}
			if err != nil {
				t.Errorf("expected valid, got error: %v", err)
			}
		})
	}
}

// TestBreakdownLimitRequiresBreakdowns exercises the breakdown_limit_requires_breakdowns
// CEL rule: a non-zero breakdown_limit without any breakdowns is a no-op (likely a client mistake).
func TestBreakdownLimitRequiresBreakdowns(t *testing.T) {
	bd := []*insightsv1.Breakdown{{Property: proto.String("$country")}}
	tests := []struct {
		name       string
		limit      int32
		breakdowns []*insightsv1.Breakdown
		wantErr    bool
		wantRule   string
	}{
		{name: "limit_zero_no_breakdowns_accepted", limit: 0, breakdowns: nil},
		{name: "limit_zero_with_breakdowns_accepted", limit: 0, breakdowns: bd},
		{name: "limit_set_with_breakdowns_accepted", limit: 10, breakdowns: bd},
		{name: "limit_set_no_breakdowns_rejected", limit: 10, breakdowns: nil, wantErr: true, wantRule: "breakdown_limit_requires_breakdowns"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := validQueryRequest()
			req.Spec.BreakdownLimit = proto.Int32(tt.limit)
			req.Spec.Breakdowns = tt.breakdowns
			err := protovalidate.Validate(req)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected validation error, got nil")
				}
				if tt.wantRule != "" && !hasRule(err, tt.wantRule) {
					t.Errorf("expected rule %q in violations, got: %v", tt.wantRule, err)
				}
				return
			}
			if err != nil {
				t.Errorf("expected valid, got error: %v", err)
			}
		})
	}
}

// TestSegmentationNoBreakdowns exercises the segmentation_no_breakdowns CEL rule:
// segmentation insights ignore breakdowns at query-build time, so reject at the boundary.
func TestSegmentationNoBreakdowns(t *testing.T) {
	bd := []*insightsv1.Breakdown{{Property: proto.String("$country")}}
	tests := []struct {
		name        string
		insightType insightsv1.InsightType
		breakdowns  []*insightsv1.Breakdown
		wantErr     bool
		wantRule    string
	}{
		{name: "trends_with_breakdowns_accepted", insightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS, breakdowns: bd},
		{name: "funnel_with_breakdowns_accepted", insightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL, breakdowns: bd},
		{name: "retention_with_breakdowns_accepted", insightType: insightsv1.InsightType_INSIGHT_TYPE_RETENTION, breakdowns: bd},
		{name: "segmentation_no_breakdowns_accepted", insightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION, breakdowns: nil},
		{name: "segmentation_with_breakdowns_rejected", insightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION, breakdowns: bd, wantErr: true, wantRule: "segmentation_no_breakdowns"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := validQueryRequest()
			req.Spec.InsightType = tt.insightType.Enum()
			req.Spec.Breakdowns = tt.breakdowns
			// Funnel and retention require at least one event (already provided by validQueryRequest);
			// no extra setup needed.
			err := protovalidate.Validate(req)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected validation error, got nil")
				}
				if tt.wantRule != "" && !hasRule(err, tt.wantRule) {
					t.Errorf("expected rule %q in violations, got: %v", tt.wantRule, err)
				}
				return
			}
			if err != nil {
				t.Errorf("expected valid, got error: %v", err)
			}
		})
	}
}

// TestNumericAggOnlyTrendsSegmentation exercises the numeric_agg_only_trends_segmentation CEL rule:
// SUM/AVG/MIN/MAX are only meaningful for trends and segmentation; funnel and retention reject them.
func TestNumericAggOnlyTrendsSegmentation(t *testing.T) {
	tests := []struct {
		name        string
		insightType insightsv1.InsightType
		agg         insightsv1.AggregationType
		wantErr     bool
		wantRule    string
	}{
		{name: "trends_SUM_accepted", insightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS, agg: insightsv1.AggregationType_AGGREGATION_TYPE_SUM},
		{name: "trends_AVG_accepted", insightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS, agg: insightsv1.AggregationType_AGGREGATION_TYPE_AVG},
		{name: "segmentation_MIN_accepted", insightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION, agg: insightsv1.AggregationType_AGGREGATION_TYPE_MIN},
		{name: "funnel_SUM_rejected", insightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL, agg: insightsv1.AggregationType_AGGREGATION_TYPE_SUM, wantErr: true, wantRule: "numeric_agg_only_trends_segmentation"},
		{name: "retention_MAX_rejected", insightType: insightsv1.InsightType_INSIGHT_TYPE_RETENTION, agg: insightsv1.AggregationType_AGGREGATION_TYPE_MAX, wantErr: true, wantRule: "numeric_agg_only_trends_segmentation"},
		// TOTAL is non-numeric; allowed on every insight type.
		{name: "funnel_TOTAL_accepted", insightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL, agg: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := validQueryRequest()
			req.Spec.InsightType = tt.insightType.Enum()
			req.Spec.Events = []*insightsv1.EventQuery{{
				Event:               &commonv1.EventFilter{Kind: proto.String("purchase")},
				Aggregation:         tt.agg.Enum(),
				AggregationProperty: proto.String("amount"), // satisfies property_required_for_numeric_agg
			}}
			err := protovalidate.Validate(req)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected validation error, got nil")
				}
				if tt.wantRule != "" && !hasRule(err, tt.wantRule) {
					t.Errorf("expected rule %q in violations, got: %v", tt.wantRule, err)
				}
				return
			}
			if err != nil {
				t.Errorf("expected valid, got error: %v", err)
			}
		})
	}
}

func TestSessionQueryValidation(t *testing.T) {
	validSessionReq := func() *insightsv1.QueryRequest {
		req := validQueryRequest()
		req.Spec.Events = nil
		req.Spec.Session = &insightsv1.SessionQuery{
			Metric: insightsv1.SessionMetric_SESSION_METRIC_SESSIONS.Enum(),
		}
		return req
	}

	cases := []struct {
		name     string
		mutate   func(*insightsv1.QueryRequest)
		wantRule string
	}{
		{"valid_trends_sessions", func(*insightsv1.QueryRequest) {}, ""},
		{"valid_segmentation_avg_duration", func(req *insightsv1.QueryRequest) {
			req.Spec.InsightType = insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION.Enum()
			req.Spec.Session.Metric = insightsv1.SessionMetric_SESSION_METRIC_AVG_DURATION.Enum()
		}, ""},
		{"session_rejects_events", func(req *insightsv1.QueryRequest) {
			req.Spec.Events = []*insightsv1.EventQuery{{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}}}
		}, "session_no_events"},
		{"session_rejects_funnel", func(req *insightsv1.QueryRequest) {
			req.Spec.InsightType = insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum()
		}, "session_only_trends_segmentation"},
		{"entry_requires_breakdown", func(req *insightsv1.QueryRequest) {
			req.Spec.Session.Metric = insightsv1.SessionMetric_SESSION_METRIC_ENTRY.Enum()
		}, "session_page_metrics_require_trends_breakdown"},
		{"entry_accepts_trends_breakdown", func(req *insightsv1.QueryRequest) {
			req.Spec.Session.Metric = insightsv1.SessionMetric_SESSION_METRIC_ENTRY.Enum()
			req.Spec.Breakdowns = []*insightsv1.Breakdown{{Property: proto.String("$url")}}
		}, ""},
		{"entry_rejects_two_breakdowns", func(req *insightsv1.QueryRequest) {
			req.Spec.Session.Metric = insightsv1.SessionMetric_SESSION_METRIC_ENTRY.Enum()
			req.Spec.Breakdowns = []*insightsv1.Breakdown{
				{Property: proto.String("$url")},
				{Property: proto.String("$country")},
			}
		}, "session_page_metrics_require_trends_breakdown"},
		{"exit_rejects_segmentation", func(req *insightsv1.QueryRequest) {
			req.Spec.InsightType = insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION.Enum()
			req.Spec.Session.Metric = insightsv1.SessionMetric_SESSION_METRIC_EXIT.Enum()
			req.Spec.Breakdowns = []*insightsv1.Breakdown{{Property: proto.String("$url")}}
		}, "session_page_metrics_require_trends_breakdown"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := validSessionReq()
			c.mutate(req)
			err := protovalidate.Validate(req)
			if c.wantRule == "" {
				if err != nil {
					t.Fatalf("expected valid, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !hasRule(err, c.wantRule) {
				t.Fatalf("expected rule %q, got %v", c.wantRule, err)
			}
		})
	}
}
