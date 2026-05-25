package seed

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/rs/xid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	coreprojects "github.com/pug-sh/pug/internal/core/projects"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
)

type tileDef struct {
	displayName      string
	description      string
	viewMode         dashboardsv1.DashboardTileViewMode
	defaultTimeRange commonv1.TimeRangePreset
	query            *insightsv1.QueryRequest
}

func (s *Seeder) seedDashboard(ctx context.Context, projectID string) error {
	slog.InfoContext(ctx, "seeding stress-test dashboard", slog.String("project_id", projectID))

	w := dbwrite.New(s.deps.pg)

	dashboardID := xid.New().String()
	if _, err := w.CreateDashboard(ctx, dbwrite.CreateDashboardParams{
		ID:          dashboardID,
		ProjectID:   projectID,
		DisplayName: "Stress Test Dashboard",
		Description: "8-tile dashboard for load testing — trends, funnels, retention, segmentation with breakdowns and filters",
	}); err != nil {
		return fmt.Errorf("create dashboard: %w", err)
	}

	now := time.Now().UTC()
	tiles := stressTiles(now)

	for i, tile := range tiles {
		content := coreprojects.InsightTile{Query: tile.query}
		enc, err := content.Encode()
		if err != nil {
			return fmt.Errorf("encode tile %q: %w", tile.displayName, err)
		}

		if _, err := w.CreateDashboardTile(ctx, dbwrite.CreateDashboardTileParams{
			ID:               xid.New().String(),
			DashboardID:      dashboardID,
			ProjectID:        projectID,
			Kind:             int16(enc.Kind),
			ViewMode:         tile.viewMode.String(),
			DefaultTimeRange: tile.defaultTimeRange.String(),
			DisplayName:      tile.displayName,
			Description:      tile.description,
			InsightQuery:     enc.InsightQuery,
			MarkdownBody:     pgtype.Text{},
			Layouts:          tileLayout(i),
		}); err != nil {
			return fmt.Errorf("create tile %q: %w", tile.displayName, err)
		}
	}

	slog.InfoContext(ctx, "stress-test dashboard seeded",
		slog.String("dashboard_id", dashboardID),
		slog.Int("tiles", len(tiles)),
	)
	return nil
}

func tileLayout(index int) map[string]any {
	col := index % 2
	row := index / 2
	return map[string]any{
		"lg": map[string]any{
			"x":      int32(col * 12),
			"y":      int32(row * 6),
			"w":      int32(12),
			"h":      int32(6),
			"minW":   int32(4),
			"maxW":   int32(24),
			"minH":   int32(3),
			"maxH":   int32(20),
			"static": false,
		},
	}
}

func ev(kind string) *insightsv1.EventQuery {
	return &insightsv1.EventQuery{
		Event:       &commonv1.EventFilter{Kind: proto.String(kind)},
		Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum(),
	}
}

func evAgg(kind string, agg insightsv1.AggregationType) *insightsv1.EventQuery {
	return &insightsv1.EventQuery{
		Event:       &commonv1.EventFilter{Kind: proto.String(kind)},
		Aggregation: agg.Enum(),
	}
}

func evSum(kind, prop string) *insightsv1.EventQuery {
	return &insightsv1.EventQuery{
		Event:               &commonv1.EventFilter{Kind: proto.String(kind)},
		Aggregation:         insightsv1.AggregationType_AGGREGATION_TYPE_SUM.Enum(),
		AggregationProperty: proto.String(prop),
	}
}

func evWithFilters(kind string, filters ...*commonv1.PropertyFilter) *insightsv1.EventQuery {
	return &insightsv1.EventQuery{
		Event:       &commonv1.EventFilter{Kind: proto.String(kind), Filters: filters},
		Aggregation: insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL.Enum(),
	}
}

func pf(prop string, op commonv1.FilterOperator, val string) *commonv1.PropertyFilter {
	return &commonv1.PropertyFilter{
		Property: proto.String(prop),
		Operator: op.Enum(),
		Value:    proto.String(val),
	}
}

func pfSet(prop string) *commonv1.PropertyFilter {
	return &commonv1.PropertyFilter{
		Property: proto.String(prop),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_IS_SET.Enum(),
	}
}

func pfIn(prop string, vals ...string) *commonv1.PropertyFilter {
	return &commonv1.PropertyFilter{
		Property: proto.String(prop),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_IN.Enum(),
		Values:   vals,
	}
}

func pfBetween(prop, lo, hi string) *commonv1.PropertyFilter {
	return &commonv1.PropertyFilter{
		Property: proto.String(prop),
		Operator: commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN.Enum(),
		Values:   []string{lo, hi},
	}
}

func fg(op commonv1.LogicalOperator, filters ...*commonv1.PropertyFilter) *insightsv1.FilterGroup {
	return &insightsv1.FilterGroup{
		Filters:  filters,
		Operator: op.Enum(),
	}
}

func bd(prop string) *insightsv1.Breakdown {
	return &insightsv1.Breakdown{Property: proto.String(prop)}
}

func tr(from, to time.Time) *commonv1.TimeRange {
	return &commonv1.TimeRange{
		From: timestamppb.New(from),
		To:   timestamppb.New(to),
	}
}

func stressTiles(now time.Time) []tileDef {
	ago90 := now.Add(-90 * 24 * time.Hour)
	ago30 := now.Add(-30 * 24 * time.Hour)
	ago14 := now.Add(-7 * 24 * time.Hour)

	and := commonv1.LogicalOperator_LOGICAL_OPERATOR_AND

	return []tileDef{
		{
			displayName:      "Web Traffic by Country & Browser",
			description:      "page_view + click + scroll across countries and browsers, filtered to web/Mac OS X or UTM-tagged",
			viewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE,
			defaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_90_DAYS,
			query: &insightsv1.QueryRequest{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
				Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
				TimeRange:   tr(ago90, now),
				Events: []*insightsv1.EventQuery{
					ev("page_view"),
					ev("click"),
					evAgg("scroll", insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS),
				},
				Breakdowns:     []*insightsv1.Breakdown{bd("$country"), bd("$browser")},
				BreakdownLimit: proto.Int32(10),
				FilterGroups: []*insightsv1.FilterGroup{
					fg(and,
						pf("$platform", commonv1.FilterOperator_FILTER_OPERATOR_EQUALS, "web"),
						pf("$os", commonv1.FilterOperator_FILTER_OPERATOR_EQUALS, "Mac OS X"),
					),
					fg(and, pfSet("$utmSource")),
				},
				FilterGroupsOperator: commonv1.LogicalOperator_LOGICAL_OPERATOR_OR.Enum(),
			},
		},

		{
			displayName:      "Revenue by Country",
			description:      "SUM of checkout amount by country, web + iOS only",
			viewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_STACKED,
			defaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS,
			query: &insightsv1.QueryRequest{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
				Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
				TimeRange:   tr(ago30, now),
				Events:      []*insightsv1.EventQuery{evSum("checkout_completed", "amount")},
				Breakdowns:  []*insightsv1.Breakdown{bd("$country")},
				BreakdownLimit: proto.Int32(15),
				FilterGroups: []*insightsv1.FilterGroup{
					fg(and,
						pfIn("$platform", "web", "ios"),
						pfBetween("amount", "10", "500"),
					),
				},
			},
		},

		{
			displayName:      "Engagement per User by City",
			description:      "Per-user average page_view and click by city, hourly over 14 days",
			viewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_AREA,
			defaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_14_DAYS,
			query: &insightsv1.QueryRequest{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
				Granularity: insightsv1.Granularity_GRANULARITY_HOUR.Enum(),
				TimeRange:   tr(ago14, now),
				Events: []*insightsv1.EventQuery{
					evAgg("page_view", insightsv1.AggregationType_AGGREGATION_TYPE_PER_USER_AVG),
					evAgg("click", insightsv1.AggregationType_AGGREGATION_TYPE_PER_USER_AVG),
				},
				Breakdowns:     []*insightsv1.Breakdown{bd("$city")},
				BreakdownLimit: proto.Int32(10),
			},
		},

		{
			displayName:      "Checkout Funnel with Timing",
			description:      "5-step checkout funnel with step timing, 24h conversion window, broken down by country and platform",
			viewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_GROUPED,
			defaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_90_DAYS,
			query: &insightsv1.QueryRequest{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
				Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
				TimeRange:   tr(ago90, now),
				Events: []*insightsv1.EventQuery{
					ev("page_view"), ev("click"), ev("add_to_cart"),
					ev("checkout_started"), ev("checkout_completed"),
				},
				Breakdowns:       []*insightsv1.Breakdown{bd("$country"), bd("$platform")},
				BreakdownLimit:   proto.Int32(10),
				ConversionWindow: durationpb.New(24 * time.Hour),
				IncludeStepTiming: proto.Bool(true),
			},
		},

		{
			displayName:      "Signup Funnel from Pricing",
			description:      "4-step signup funnel, page_view filtered to /pricing, 1h window, browser breakdown",
			viewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_TABLE,
			defaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS,
			query: &insightsv1.QueryRequest{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
				Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
				TimeRange:   tr(ago30, now),
				Events: []*insightsv1.EventQuery{
					evWithFilters("page_view",
						pf("$url", commonv1.FilterOperator_FILTER_OPERATOR_CONTAINS, "/pricing"),
					),
					ev("form_start"), ev("form_submit"), ev("signup"),
				},
				Breakdowns:       []*insightsv1.Breakdown{bd("$browser")},
				BreakdownLimit:   proto.Int32(5),
				ConversionWindow: durationpb.New(time.Hour),
				IncludeStepTiming: proto.Bool(true),
				FilterGroups: []*insightsv1.FilterGroup{
					fg(and, pf("$platform", commonv1.FilterOperator_FILTER_OPERATOR_EQUALS, "web")),
				},
			},
		},

		{
			displayName:      "Click Retention by Platform",
			description:      "Weekly retention: page_view → click, broken down by platform",
			viewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_TABLE,
			defaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_90_DAYS,
			query: &insightsv1.QueryRequest{
				InsightType:    insightsv1.InsightType_INSIGHT_TYPE_RETENTION.Enum(),
				Granularity:    insightsv1.Granularity_GRANULARITY_WEEK.Enum(),
				TimeRange:      tr(ago90, now),
				Events:         []*insightsv1.EventQuery{ev("page_view"), ev("click")},
				Breakdowns:     []*insightsv1.Breakdown{bd("$platform")},
				BreakdownLimit: proto.Int32(5),
			},
		},

		{
			displayName:      "Purchase Retention by Country",
			description:      "Daily retention: app_open → checkout_completed, mobile only, by country",
			viewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_TABLE,
			defaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS,
			query: &insightsv1.QueryRequest{
				InsightType:    insightsv1.InsightType_INSIGHT_TYPE_RETENTION.Enum(),
				Granularity:    insightsv1.Granularity_GRANULARITY_DAY.Enum(),
				TimeRange:      tr(ago30, now),
				Events:         []*insightsv1.EventQuery{ev("app_open"), ev("checkout_completed")},
				Breakdowns:     []*insightsv1.Breakdown{bd("$country")},
				BreakdownLimit: proto.Int32(10),
				FilterGroups: []*insightsv1.FilterGroup{
					fg(and, pf("$platform", commonv1.FilterOperator_FILTER_OPERATOR_NOT_EQUALS, "web")),
				},
			},
		},

		{
			displayName:      "Revenue Trends (US Web + GB Chrome + iOS)",
			description:      "Checkout revenue over time across three audience segments via OR filter groups",
			viewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE,
			defaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_90_DAYS,
			query: &insightsv1.QueryRequest{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
				Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
				TimeRange:   tr(ago90, now),
				Events:      []*insightsv1.EventQuery{evSum("checkout_completed", "amount")},
				Breakdowns:  []*insightsv1.Breakdown{bd("$country")},
				BreakdownLimit: proto.Int32(10),
				FilterGroups: []*insightsv1.FilterGroup{
					fg(and,
						pf("$platform", commonv1.FilterOperator_FILTER_OPERATOR_EQUALS, "web"),
						pf("$country", commonv1.FilterOperator_FILTER_OPERATOR_EQUALS, "US"),
					),
					fg(and,
						pf("$country", commonv1.FilterOperator_FILTER_OPERATOR_EQUALS, "GB"),
						pf("$browser", commonv1.FilterOperator_FILTER_OPERATOR_EQUALS, "Chrome"),
					),
					fg(and,
						pf("$os", commonv1.FilterOperator_FILTER_OPERATOR_CONTAINS, "iOS"),
					),
				},
				FilterGroupsOperator: commonv1.LogicalOperator_LOGICAL_OPERATOR_OR.Enum(),
			},
		},
	}
}
