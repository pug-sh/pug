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

// TileForBench is a benchmark-only view of a seeded tile.
// Used by cmd/dashboard-bench to time each tile's compiled query against
// a live ClickHouse without going through the RPC handler.
type TileForBench struct{ inner tileDef }

func (t TileForBench) DisplayName() string                  { return t.inner.displayName }
func (t TileForBench) Query() *insightsv1.QueryRequest      { return t.inner.query }

// StressTrendsTilesForBench returns the trends-only tiles built into the
// stress-test dashboard. Exposed so the benchmark binary can exercise the
// exact same queries the seeded dashboard renders.
func StressTrendsTilesForBench(now time.Time) []TileForBench {
	return tilesForBench(trendsTiles(now))
}

// StressFunnelTilesForBench returns the funnel tiles built into the funnel stress dashboard.
func StressFunnelTilesForBench(now time.Time) []TileForBench {
	return tilesForBench(funnelTiles(now))
}

// StressRetentionTilesForBench returns the retention tiles built into the retention stress dashboard.
func StressRetentionTilesForBench(now time.Time) []TileForBench {
	return tilesForBench(retentionTiles(now))
}

func tilesForBench(defs []tileDef) []TileForBench {
	out := make([]TileForBench, len(defs))
	for i := range defs {
		out[i] = TileForBench{inner: defs[i]}
	}
	return out
}

func (s *Seeder) seedDashboard(ctx context.Context, projectID string) error {
	slog.InfoContext(ctx, "seeding stress-test dashboards", slog.String("project_id", projectID))

	if _, err := s.deps.pg.Exec(ctx,
		`delete from dashboards where project_id = $1`, projectID); err != nil {
		return fmt.Errorf("delete existing dashboards: %w", err)
	}

	w := dbwrite.New(s.deps.pg)
	now := time.Now().UTC()

	specs := []struct {
		name  string
		desc  string
		tiles []tileDef
	}{
		{
			name:  "Trends Stress Test",
			desc:  "8 complex trends tiles — multi-event, breakdowns, filters, all granularities",
			tiles: trendsTiles(now),
		},
		{
			name:  "Funnel Stress Test",
			desc:  "8 complex funnel tiles — multi-step, breakdowns, filters, counts + step timing",
			tiles: funnelTiles(now),
		},
		{
			name:  "Retention Stress Test",
			desc:  "8 complex retention tiles — cohort/return pairs, breakdowns, filters, all granularities",
			tiles: retentionTiles(now),
		},
	}

	for _, spec := range specs {
		dashboardID, err := s.createDashboardWithTiles(ctx, w, projectID, spec.name, spec.desc, spec.tiles)
		if err != nil {
			return err
		}
		slog.InfoContext(ctx, "stress-test dashboard seeded",
			slog.String("dashboard_id", dashboardID),
			slog.String("display_name", spec.name),
			slog.Int("tiles", len(spec.tiles)),
		)
	}
	return nil
}

func (s *Seeder) createDashboardWithTiles(
	ctx context.Context,
	w *dbwrite.Queries,
	projectID, displayName, description string,
	tiles []tileDef,
) (string, error) {
	dashboardID := xid.New().String()
	if _, err := w.CreateDashboard(ctx, dbwrite.CreateDashboardParams{
		ID:          dashboardID,
		ProjectID:   projectID,
		DisplayName: displayName,
		Description: description,
	}); err != nil {
		return "", fmt.Errorf("create dashboard %q: %w", displayName, err)
	}

	for i, tile := range tiles {
		content := coreprojects.InsightTile{Query: tile.query}
		enc, err := content.Encode()
		if err != nil {
			return "", fmt.Errorf("encode tile %q: %w", tile.displayName, err)
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
			return "", fmt.Errorf("create tile %q: %w", tile.displayName, err)
		}
	}
	return dashboardID, nil
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

// trendsTiles returns 8 complex trends-only tiles, each within its granularity's
// allowed time-range cap (MINUTE<=6h, HOUR<=14d, DAY<=365d, WEEK<=1461d, MONTH<=3652d).
func trendsTiles(now time.Time) []tileDef {
	ago6h := now.Add(-6 * time.Hour)
	ago7d := now.Add(-7 * 24 * time.Hour)
	ago30d := now.Add(-30 * 24 * time.Hour)
	ago90d := now.Add(-90 * 24 * time.Hour)
	ago180d := now.Add(-180 * 24 * time.Hour)

	and := commonv1.LogicalOperator_LOGICAL_OPERATOR_AND
	or := commonv1.LogicalOperator_LOGICAL_OPERATOR_OR

	return []tileDef{
		// 1) Multi-event daily totals + uniques across two breakdowns and two OR'd filter groups
		{
			displayName:      "Web Traffic by Country & Browser",
			description:      "page_view total + click total + scroll uniques over 90d, filtered to Mac web OR utm-tagged traffic",
			viewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE,
			defaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_90_DAYS,
			query: &insightsv1.QueryRequest{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
				Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
				TimeRange:   tr(ago90d, now),
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
				FilterGroupsOperator: or.Enum(),
			},
		},

		// 2) Revenue SUM aggregation over a numeric custom property, grouped by country
		{
			displayName:      "Revenue by Country (web + iOS)",
			description:      "SUM of checkout_completed.amount over 30d, broken down by country, restricted to web/iOS",
			viewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_STACKED,
			defaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS,
			query: &insightsv1.QueryRequest{
				InsightType:    insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
				Granularity:    insightsv1.Granularity_GRANULARITY_DAY.Enum(),
				TimeRange:      tr(ago30d, now),
				Events:         []*insightsv1.EventQuery{evSum("checkout_completed", "amount")},
				Breakdowns:     []*insightsv1.Breakdown{bd("$country")},
				BreakdownLimit: proto.Int32(15),
				FilterGroups: []*insightsv1.FilterGroup{
					fg(and, pfIn("$platform", "web", "ios")),
				},
			},
		},

		// 3) Per-user engagement, hourly, with city breakdown (hits the 14-day cap exactly at 7d)
		{
			displayName:      "Engagement per User by City (hourly)",
			description:      "Per-user average page_view + click, hourly over the last 7 days, top cities",
			viewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_AREA,
			defaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS,
			query: &insightsv1.QueryRequest{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
				Granularity: insightsv1.Granularity_GRANULARITY_HOUR.Enum(),
				TimeRange:   tr(ago7d, now),
				Events: []*insightsv1.EventQuery{
					evAgg("page_view", insightsv1.AggregationType_AGGREGATION_TYPE_PER_USER_AVG),
					evAgg("click", insightsv1.AggregationType_AGGREGATION_TYPE_PER_USER_AVG),
				},
				Breakdowns:     []*insightsv1.Breakdown{bd("$city")},
				BreakdownLimit: proto.Int32(10),
			},
		},

		// 4) Realtime activity, minute granularity, last 6 hours, platform breakdown
		{
			displayName:      "Realtime Activity by Platform (last 6h)",
			description:      "Per-minute unique users across page_view + click for the last 6 hours, by platform",
			viewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE,
			defaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_6_HOURS,
			query: &insightsv1.QueryRequest{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
				Granularity: insightsv1.Granularity_GRANULARITY_MINUTE.Enum(),
				TimeRange:   tr(ago6h, now),
				Events: []*insightsv1.EventQuery{
					evAgg("page_view", insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS),
					evAgg("click", insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS),
				},
				Breakdowns:     []*insightsv1.Breakdown{bd("$platform")},
				BreakdownLimit: proto.Int32(5),
			},
		},

		// 5) Five-event funnel-shape trend (multi-event UNION ALL stress, no breakdown)
		{
			displayName:      "Checkout Funnel Volumes Over Time",
			description:      "Daily totals for the 5 checkout funnel events over 90 days",
			viewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_STACKED,
			defaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_90_DAYS,
			query: &insightsv1.QueryRequest{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
				Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
				TimeRange:   tr(ago90d, now),
				Events: []*insightsv1.EventQuery{
					ev("page_view"),
					ev("add_to_cart"),
					ev("checkout_started"),
					ev("checkout_completed"),
					evAgg("checkout_completed", insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS),
				},
			},
		},

		// 6) UTM mix — two breakdowns + IS_SET filter, 90 days
		{
			displayName:      "Acquisition Mix by UTM Source & Medium",
			description:      "page_view + signup totals over 90d, broken down by utm source + medium, only attributed traffic",
			viewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_GROUPED,
			defaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_90_DAYS,
			query: &insightsv1.QueryRequest{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
				Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
				TimeRange:   tr(ago90d, now),
				Events: []*insightsv1.EventQuery{
					ev("page_view"),
					ev("signup"),
				},
				Breakdowns:     []*insightsv1.Breakdown{bd("$utmSource"), bd("$utmMedium")},
				BreakdownLimit: proto.Int32(10),
				FilterGroups: []*insightsv1.FilterGroup{
					fg(and, pfSet("$utmSource"), pfSet("$utmMedium")),
				},
			},
		},

		// 7) Weekly mobile vs web unique users by OS, ~90 days
		{
			displayName:      "Weekly Unique Users by OS",
			description:      "Weekly unique users on app_open broken down by OS, last 90 days",
			viewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE,
			defaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_90_DAYS,
			query: &insightsv1.QueryRequest{
				InsightType:    insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
				Granularity:    insightsv1.Granularity_GRANULARITY_WEEK.Enum(),
				TimeRange:      tr(ago90d, now),
				Events:         []*insightsv1.EventQuery{evAgg("app_open", insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS)},
				Breakdowns:     []*insightsv1.Breakdown{bd("$os")},
				BreakdownLimit: proto.Int32(8),
			},
		},

		// 8) Half-year purchaser cohort across three audience segments (OR'd filter groups, breakdown by country)
		{
			displayName:      "Purchasers by Country across Audience Segments",
			description:      "Daily unique purchasers over ~180d, broken down by country, across US-web OR GB-Chrome OR iOS audiences",
			viewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE,
			defaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_180_DAYS,
			query: &insightsv1.QueryRequest{
				InsightType:    insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
				Granularity:    insightsv1.Granularity_GRANULARITY_DAY.Enum(),
				TimeRange:      tr(ago180d, now),
				Events:         []*insightsv1.EventQuery{evAgg("checkout_completed", insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS)},
				Breakdowns:     []*insightsv1.Breakdown{bd("$country")},
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
					fg(and, pf("$os", commonv1.FilterOperator_FILTER_OPERATOR_CONTAINS, "iOS")),
				},
				FilterGroupsOperator: or.Enum(),
			},
		},
	}
}

func funnelStep(kind string) *insightsv1.EventQuery {
	return &insightsv1.EventQuery{
		Event: &commonv1.EventFilter{Kind: proto.String(kind)},
	}
}

func funnelStepFiltered(kind string, filters ...*commonv1.PropertyFilter) *insightsv1.EventQuery {
	return &insightsv1.EventQuery{
		Event: &commonv1.EventFilter{Kind: proto.String(kind), Filters: filters},
	}
}

// funnelTiles returns 8 complex funnel tiles mixing counts-only and step-timing queries.
// Time ranges are kept at 7–30 days so windowFunnel scans stay bounded on large datasets.
func funnelTiles(now time.Time) []tileDef {
	ago7d := now.Add(-7 * 24 * time.Hour)
	ago14d := now.Add(-14 * 24 * time.Hour)
	ago30d := now.Add(-30 * 24 * time.Hour)

	and := commonv1.LogicalOperator_LOGICAL_OPERATOR_AND
	or := commonv1.LogicalOperator_LOGICAL_OPERATOR_OR

	return []tileDef{
		// 1) Simple 3-step checkout, counts only, tight 1h conversion window
		{
			displayName:      "Quick Checkout (3-step)",
			description:      "page_view → add_to_cart → checkout_completed over 7d with 1h conversion window",
			viewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_GROUPED,
			defaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS,
			query: &insightsv1.QueryRequest{
				InsightType:      insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
				Granularity:      insightsv1.Granularity_GRANULARITY_DAY.Enum(),
				TimeRange:        tr(ago7d, now),
				Events:           []*insightsv1.EventQuery{funnelStep("page_view"), funnelStep("add_to_cart"), funnelStep("checkout_completed")},
				ConversionWindow: durationpb.New(time.Hour),
			},
		},

		// 2) Browse engagement micro-funnel with country breakdown
		{
			displayName:      "Browse to Click by Country",
			description:      "page_view → click over 14d, broken down by country",
			viewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_GROUPED,
			defaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_14_DAYS,
			query: &insightsv1.QueryRequest{
				InsightType:    insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
				Granularity:    insightsv1.Granularity_GRANULARITY_DAY.Enum(),
				TimeRange:      tr(ago14d, now),
				Events:         []*insightsv1.EventQuery{funnelStep("page_view"), funnelStep("click")},
				Breakdowns:     []*insightsv1.Breakdown{bd("$country")},
				BreakdownLimit: proto.Int32(10),
			},
		},

		// 3) Web checkout funnel with platform filter and country breakdown
		{
			displayName:      "Web Checkout by Country",
			description:      "4-step web checkout over 30d, 24h window, broken down by country",
			viewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_STACKED,
			defaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS,
			query: &insightsv1.QueryRequest{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
				Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
				TimeRange:   tr(ago30d, now),
				Events: []*insightsv1.EventQuery{
					funnelStep("page_view"), funnelStep("add_to_cart"),
					funnelStep("checkout_started"), funnelStep("checkout_completed"),
				},
				Breakdowns:       []*insightsv1.Breakdown{bd("$country")},
				BreakdownLimit:   proto.Int32(10),
				ConversionWindow: durationpb.New(24 * time.Hour),
				FilterGroups: []*insightsv1.FilterGroup{
					fg(and, pf("$platform", commonv1.FilterOperator_FILTER_OPERATOR_EQUALS, "web")),
				},
			},
		},

		// 4) Mobile app purchase funnel, platform breakdown
		{
			displayName:      "Mobile Purchase Funnel",
			description:      "app_open → page_view → add_to_cart → checkout_completed, mobile-only, 7d window",
			viewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_GROUPED,
			defaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS,
			query: &insightsv1.QueryRequest{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
				Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
				TimeRange:   tr(ago30d, now),
				Events: []*insightsv1.EventQuery{
					funnelStep("app_open"), funnelStep("page_view"),
					funnelStep("add_to_cart"), funnelStep("checkout_completed"),
				},
				Breakdowns:       []*insightsv1.Breakdown{bd("$platform")},
				BreakdownLimit:   proto.Int32(5),
				ConversionWindow: durationpb.New(7 * 24 * time.Hour),
				FilterGroups: []*insightsv1.FilterGroup{
					fg(and, pf("$platform", commonv1.FilterOperator_FILTER_OPERATOR_NOT_EQUALS, "web")),
				},
			},
		},

		// 5) Full 5-step checkout with step timing, browser breakdown + web filter
		{
			displayName:      "Checkout Funnel with Timing",
			description:      "5-step web checkout with step timing, 24h window, broken down by browser",
			viewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_GROUPED,
			defaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS,
			query: &insightsv1.QueryRequest{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
				Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
				TimeRange:   tr(ago30d, now),
				Events: []*insightsv1.EventQuery{
					funnelStep("page_view"), funnelStep("click"), funnelStep("add_to_cart"),
					funnelStep("checkout_started"), funnelStep("checkout_completed"),
				},
				Breakdowns:        []*insightsv1.Breakdown{bd("$browser")},
				BreakdownLimit:    proto.Int32(5),
				ConversionWindow:  durationpb.New(24 * time.Hour),
				IncludeStepTiming: proto.Bool(true),
				FilterGroups: []*insightsv1.FilterGroup{
					fg(and, pf("$platform", commonv1.FilterOperator_FILTER_OPERATOR_EQUALS, "web")),
				},
			},
		},

		// 6) Signup funnel from pricing page with browser breakdown + timing
		{
			displayName:      "Signup from Pricing with Timing",
			description:      "Pricing page_view → form_start → form_submit → signup, 1h window, by browser",
			viewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_TABLE,
			defaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS,
			query: &insightsv1.QueryRequest{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
				Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
				TimeRange:   tr(ago30d, now),
				Events: []*insightsv1.EventQuery{
					funnelStepFiltered("page_view",
						pf("$url", commonv1.FilterOperator_FILTER_OPERATOR_CONTAINS, "/pricing")),
					funnelStep("form_start"), funnelStep("form_submit"), funnelStep("signup"),
				},
				Breakdowns:        []*insightsv1.Breakdown{bd("$browser")},
				BreakdownLimit:    proto.Int32(5),
				ConversionWindow:  durationpb.New(time.Hour),
				IncludeStepTiming: proto.Bool(true),
				FilterGroups: []*insightsv1.FilterGroup{
					fg(and, pf("$platform", commonv1.FilterOperator_FILTER_OPERATOR_EQUALS, "web")),
				},
			},
		},

		// 7) Multi-segment checkout with OR filter groups + timing (14d to bound scan)
		{
			displayName:      "Segmented Checkout with Timing",
			description:      "5-step checkout with timing across US-web / GB-Chrome / iOS segments, by country",
			viewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_GROUPED,
			defaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_14_DAYS,
			query: &insightsv1.QueryRequest{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
				Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
				TimeRange:   tr(ago14d, now),
				Events: []*insightsv1.EventQuery{
					funnelStep("page_view"), funnelStep("click"), funnelStep("add_to_cart"),
					funnelStep("checkout_started"), funnelStep("checkout_completed"),
				},
				Breakdowns:        []*insightsv1.Breakdown{bd("$country")},
				BreakdownLimit:    proto.Int32(10),
				ConversionWindow:  durationpb.New(24 * time.Hour),
				IncludeStepTiming: proto.Bool(true),
				FilterGroups: []*insightsv1.FilterGroup{
					fg(and,
						pf("$platform", commonv1.FilterOperator_FILTER_OPERATOR_EQUALS, "web"),
						pf("$country", commonv1.FilterOperator_FILTER_OPERATOR_EQUALS, "US"),
					),
					fg(and,
						pf("$country", commonv1.FilterOperator_FILTER_OPERATOR_EQUALS, "GB"),
						pf("$browser", commonv1.FilterOperator_FILTER_OPERATOR_EQUALS, "Chrome"),
					),
					fg(and, pf("$os", commonv1.FilterOperator_FILTER_OPERATOR_CONTAINS, "iOS")),
				},
				FilterGroupsOperator: or.Enum(),
			},
		},

		// 8) UTM-attributed checkout with utm source breakdown + timing
		{
			displayName:      "UTM Checkout with Timing",
			description:      "4-step checkout with timing, utm-attributed traffic only, by utm source + medium",
			viewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_STACKED,
			defaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS,
			query: &insightsv1.QueryRequest{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
				Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
				TimeRange:   tr(ago30d, now),
				Events: []*insightsv1.EventQuery{
					funnelStep("page_view"), funnelStep("add_to_cart"),
					funnelStep("checkout_started"), funnelStep("checkout_completed"),
				},
				Breakdowns:        []*insightsv1.Breakdown{bd("$utmSource"), bd("$utmMedium")},
				BreakdownLimit:    proto.Int32(10),
				ConversionWindow:  durationpb.New(24 * time.Hour),
				IncludeStepTiming: proto.Bool(true),
				FilterGroups: []*insightsv1.FilterGroup{
					fg(and, pfSet("$utmSource"), pfSet("$utmMedium")),
				},
			},
		},
	}
}

func retentionEvent(kind string) *insightsv1.EventQuery {
	return &insightsv1.EventQuery{
		Event: &commonv1.EventFilter{Kind: proto.String(kind)},
	}
}

func retentionPair(start, ret string) []*insightsv1.EventQuery {
	return []*insightsv1.EventQuery{retentionEvent(start), retentionEvent(ret)}
}

// retentionTiles returns 8 complex retention tiles within granularity time-range caps.
// Retention multiplies cohort × follow-up buckets (triangular), so daily ranges stay ≤90d
// and weekly tiles use ≤90d to keep row counts bounded on large datasets.
func retentionTiles(now time.Time) []tileDef {
	ago7d := now.Add(-7 * 24 * time.Hour)
	ago14d := now.Add(-14 * 24 * time.Hour)
	ago30d := now.Add(-30 * 24 * time.Hour)
	ago90d := now.Add(-90 * 24 * time.Hour)

	and := commonv1.LogicalOperator_LOGICAL_OPERATOR_AND
	or := commonv1.LogicalOperator_LOGICAL_OPERATOR_OR

	return []tileDef{
		// 1) Basic activation retention, daily cohorts over 30d
		{
			displayName:      "Signup to Page View",
			description:      "Daily retention from signup → page_view over 30 days",
			viewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_TABLE,
			defaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS,
			query: &insightsv1.QueryRequest{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_RETENTION.Enum(),
				Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
				TimeRange:   tr(ago30d, now),
				Events:      retentionPair("signup", "page_view"),
			},
		},

		// 2) Activation by country
		{
			displayName:      "Signup to App Open by Country",
			description:      "signup → app_open daily retention over 30d, broken down by country",
			viewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_TABLE,
			defaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS,
			query: &insightsv1.QueryRequest{
				InsightType:    insightsv1.InsightType_INSIGHT_TYPE_RETENTION.Enum(),
				Granularity:    insightsv1.Granularity_GRANULARITY_DAY.Enum(),
				TimeRange:      tr(ago30d, now),
				Events:         retentionPair("signup", "app_open"),
				Breakdowns:     []*insightsv1.Breakdown{bd("$country")},
				BreakdownLimit: proto.Int32(10),
			},
		},

		// 3) App stickiness (same event return), platform breakdown
		{
			displayName:      "App Open Stickiness by Platform",
			description:      "app_open → app_open daily retention over 14d, broken down by platform",
			viewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_TABLE,
			defaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_14_DAYS,
			query: &insightsv1.QueryRequest{
				InsightType:    insightsv1.InsightType_INSIGHT_TYPE_RETENTION.Enum(),
				Granularity:    insightsv1.Granularity_GRANULARITY_DAY.Enum(),
				TimeRange:      tr(ago14d, now),
				Events:         retentionPair("app_open", "app_open"),
				Breakdowns:     []*insightsv1.Breakdown{bd("$platform")},
				BreakdownLimit: proto.Int32(5),
			},
		},

		// 4) Purchaser retention with web filter + country breakdown
		{
			displayName:      "Purchaser Retention by Country",
			description:      "signup → checkout_completed daily retention over 30d, web-only, by country",
			viewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_TABLE,
			defaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS,
			query: &insightsv1.QueryRequest{
				InsightType:    insightsv1.InsightType_INSIGHT_TYPE_RETENTION.Enum(),
				Granularity:    insightsv1.Granularity_GRANULARITY_DAY.Enum(),
				TimeRange:      tr(ago30d, now),
				Events:         retentionPair("signup", "checkout_completed"),
				Breakdowns:     []*insightsv1.Breakdown{bd("$country")},
				BreakdownLimit: proto.Int32(10),
				FilterGroups: []*insightsv1.FilterGroup{
					fg(and, pf("$platform", commonv1.FilterOperator_FILTER_OPERATOR_EQUALS, "web")),
				},
			},
		},

		// 5) Browse engagement, hourly granularity (7d cap for HOUR)
		{
			displayName:      "Browse Engagement (hourly)",
			description:      "page_view → click hourly retention over 7 days, broken down by browser",
			viewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_TABLE,
			defaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_7_DAYS,
			query: &insightsv1.QueryRequest{
				InsightType:    insightsv1.InsightType_INSIGHT_TYPE_RETENTION.Enum(),
				Granularity:    insightsv1.Granularity_GRANULARITY_HOUR.Enum(),
				TimeRange:      tr(ago7d, now),
				Events:         retentionPair("page_view", "click"),
				Breakdowns:     []*insightsv1.Breakdown{bd("$browser")},
				BreakdownLimit: proto.Int32(5),
			},
		},

		// 6) Mobile return rate, weekly cohorts over 90d
		{
			displayName:      "Mobile Return Rate (weekly)",
			description:      "app_open → page_view weekly retention over 90d, broken down by platform",
			viewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_TABLE,
			defaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_90_DAYS,
			query: &insightsv1.QueryRequest{
				InsightType:    insightsv1.InsightType_INSIGHT_TYPE_RETENTION.Enum(),
				Granularity:    insightsv1.Granularity_GRANULARITY_WEEK.Enum(),
				TimeRange:      tr(ago90d, now),
				Events:         retentionPair("app_open", "page_view"),
				Breakdowns:     []*insightsv1.Breakdown{bd("$platform")},
				BreakdownLimit: proto.Int32(5),
				FilterGroups: []*insightsv1.FilterGroup{
					fg(and, pf("$platform", commonv1.FilterOperator_FILTER_OPERATOR_NOT_EQUALS, "web")),
				},
			},
		},

		// 7) Segmented auth retention with OR filter groups
		{
			displayName:      "Segmented Auth Retention",
			description:      "signup → login daily retention over 14d across US-web / GB-Chrome / iOS segments, by country",
			viewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_TABLE,
			defaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_14_DAYS,
			query: &insightsv1.QueryRequest{
				InsightType:    insightsv1.InsightType_INSIGHT_TYPE_RETENTION.Enum(),
				Granularity:    insightsv1.Granularity_GRANULARITY_DAY.Enum(),
				TimeRange:      tr(ago14d, now),
				Events:         retentionPair("signup", "login"),
				Breakdowns:     []*insightsv1.Breakdown{bd("$country")},
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
					fg(and, pf("$os", commonv1.FilterOperator_FILTER_OPERATOR_CONTAINS, "iOS")),
				},
				FilterGroupsOperator: or.Enum(),
			},
		},

		// 8) UTM-attributed visitor return with two breakdowns
		{
			displayName:      "UTM Visitor Return",
			description:      "page_view → page_view daily retention over 30d, utm-attributed only, by utm source + medium",
			viewMode:         dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_TABLE,
			defaultTimeRange: commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS,
			query: &insightsv1.QueryRequest{
				InsightType:    insightsv1.InsightType_INSIGHT_TYPE_RETENTION.Enum(),
				Granularity:    insightsv1.Granularity_GRANULARITY_DAY.Enum(),
				TimeRange:      tr(ago30d, now),
				Events:         retentionPair("page_view", "page_view"),
				Breakdowns:     []*insightsv1.Breakdown{bd("$utmSource"), bd("$utmMedium")},
				BreakdownLimit: proto.Int32(10),
				FilterGroups: []*insightsv1.FilterGroup{
					fg(and, pfSet("$utmSource"), pfSet("$utmMedium")),
				},
			},
		},
	}
}
