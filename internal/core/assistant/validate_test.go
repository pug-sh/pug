package assistant

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
)

func testTile(spec *insightsv1.InsightQuerySpec) *dashboardsv1.DashboardTileInput {
	return &dashboardsv1.DashboardTileInput{
		DisplayName: proto.String("Test tile"),
		Content:     &dashboardsv1.DashboardTileInput_Insight{Insight: &dashboardsv1.InsightTileContent{Spec: spec}},
		Position: &dashboardsv1.GridPosition{
			X: proto.Int32(0), Y: proto.Int32(0), W: proto.Int32(36), H: proto.Int32(200),
		},
	}
}

func eventQuery(kind string) *insightsv1.EventQuery {
	return &insightsv1.EventQuery{Event: &commonv1.EventFilter{Kind: proto.String(kind)}}
}

func TestValidateTile_AcceptsWellFormedTile(t *testing.T) {
	result := validateTile(testTile(&insightsv1.InsightQuerySpec{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
		Events: []*insightsv1.EventQuery{{
			Event:       &commonv1.EventFilter{Kind: proto.String("page_view")},
			Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS.Enum(),
		}},
		Breakdowns: []*insightsv1.Breakdown{{Property: proto.String("$country")}},
	}))
	if !result.OK {
		t.Fatalf("expected ok, got violations: %s", result.Formatted)
	}
}

func TestValidateTile_RejectsFunnelWithNoEvents(t *testing.T) {
	result := validateTile(testTile(&insightsv1.InsightQuerySpec{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
	}))
	if result.OK {
		t.Fatal("expected violation")
	}
	v := result.Violations[0]
	if v.Message != "funnel and retention insight types require at least one event" {
		t.Fatalf("message = %q", v.Message)
	}
	if v.RuleID != "insight_query_spec.funnel_retention_require_events" {
		t.Fatalf("ruleID = %q", v.RuleID)
	}
}

// Without FieldPathString this renders as an opaque struct dump; the model
// needs "events[1]" to know which element to fix.
func TestValidateTile_RendersRepeatedFieldPathReadably(t *testing.T) {
	result := validateTile(testTile(&insightsv1.InsightQuerySpec{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
		Events: []*insightsv1.EventQuery{
			eventQuery("page_view"),
			{
				Event:       &commonv1.EventFilter{Kind: proto.String("purchase")},
				Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_SUM.Enum(),
			},
		},
	}))
	if result.OK {
		t.Fatal("expected violation (SUM without aggregation_property)")
	}
	if got := result.Violations[0].Path; got != "insight.spec.events[1]" {
		t.Fatalf("path = %q, want insight.spec.events[1]", got)
	}
}

func TestValidateTile_RejectsDuplicateBreakdownProperties(t *testing.T) {
	result := validateTile(testTile(&insightsv1.InsightQuerySpec{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
		Events:      []*insightsv1.EventQuery{eventQuery("page_view")},
		Breakdowns: []*insightsv1.Breakdown{
			{Property: proto.String("$country")},
			{Property: proto.String("$country")},
		},
	}))
	if result.OK {
		t.Fatal("expected violation")
	}
	if !strings.Contains(result.Formatted, "breakdown properties must be unique") {
		t.Fatalf("formatted = %q", result.Formatted)
	}
}

func TestValidateTile_RejectsTopKWithBreakdowns(t *testing.T) {
	result := validateTile(testTile(&insightsv1.InsightQuerySpec{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TOP_K.Enum(),
		TopK: &insightsv1.TopKQuery{
			Dimension: insightsv1.TopKQuery_DIMENSION_EVENT_KIND.Enum(),
		},
		Breakdowns: []*insightsv1.Breakdown{{Property: proto.String("$country")}},
	}))
	if result.OK {
		t.Fatal("expected violation")
	}
	if !strings.Contains(result.Formatted, "breakdowns are not supported for top k insight type") {
		t.Fatalf("formatted = %q", result.Formatted)
	}
}

func TestValidateTile_RejectsConflictingVisualizationOptions(t *testing.T) {
	tile := testTile(&insightsv1.InsightQuerySpec{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
		Events:      []*insightsv1.EventQuery{eventQuery("page_view")},
	})
	tile.Visualization = &dashboardsv1.VisualizationOptions{
		LogScale: proto.Bool(true), ZeroBaseline: proto.Bool(true),
	}
	result := validateTile(tile)
	if result.OK {
		t.Fatal("expected violation")
	}
	if !strings.Contains(result.Formatted, "log_scale cannot be combined with zero_baseline") {
		t.Fatalf("formatted = %q", result.Formatted)
	}
}

func TestValidateTile_RejectsIncompleteGridPosition(t *testing.T) {
	tile := testTile(&insightsv1.InsightQuerySpec{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
		Events:      []*insightsv1.EventQuery{eventQuery("page_view")},
	})
	tile.Position = &dashboardsv1.GridPosition{X: proto.Int32(0), Y: proto.Int32(0)}
	result := validateTile(tile)
	if result.OK {
		t.Fatal("expected violation")
	}
	if !strings.Contains(result.Formatted, "position requires both w (1..72) and h (1..800)") {
		t.Fatalf("formatted = %q", result.Formatted)
	}
}

func TestValidateTile_FormatsViolationsForTheModel(t *testing.T) {
	result := validateTile(testTile(&insightsv1.InsightQuerySpec{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
	}))
	if result.OK {
		t.Fatal("expected violation")
	}
	if !strings.HasPrefix(result.Formatted, "- ") {
		t.Fatalf("formatted should be a bullet list, got %q", result.Formatted)
	}
	if !strings.Contains(result.Formatted, "funnel and retention insight types require at least one event") {
		t.Fatalf("formatted = %q", result.Formatted)
	}
}

// Fail-closed. A validator error (CEL compilation/runtime) must read as a
// distinct, labelled failure — never as "no violations", which would let an
// unvalidated tile sail through as if checked.
func TestToResult_ValidatorErrorIsHardFailureNeverAPass(t *testing.T) {
	result := toResult(errors.New("unresolved attribute"))
	if result.OK {
		t.Fatal("a validator error must not read as valid")
	}
	if result.Violations[0].RuleID != "validator.error" {
		t.Fatalf("ruleID = %q", result.Violations[0].RuleID)
	}
	if !strings.Contains(result.Formatted, "validator misconfigured") {
		t.Fatalf("formatted = %q", result.Formatted)
	}
}

func TestToResult_NilErrorIsValid(t *testing.T) {
	if result := toResult(nil); !result.OK {
		t.Fatalf("got %+v", result)
	}
}
