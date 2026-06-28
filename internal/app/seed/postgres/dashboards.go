package seed

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	coredashboards "github.com/pug-sh/pug/internal/core/dashboards"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
)

// ---------------------------------------------------------------------------
// Demo dashboards
//
// These ship a set of saved dashboards on the demo project so the demo isn't a
// blank canvas: every board is built from the real seeded event/property
// vocabulary (internal/app/seed/clickhouse) and is deliberately complex, so the
// boards double as a living showcase of the insights engine — mixed-source
// nested filter groups, per-event/per-step filters, numeric aggregations,
// funnels with conversion windows and timing, retention cohorts, and top-K
// rankings (incl. USER-dimension profile enrichment). Session metrics and user
// flow (Sankey) are intentionally excluded — the dashboard FE renders neither.
//
// Capability notes baked into the tile choices below:
//   - Breakdowns and top-K dimensions read EVENT properties only (auto/custom),
//     so profile attributes (pug_club, age_years, breed) are used as FILTERS
//     (PROPERTY_SOURCE_PROFILE, compiled to a profile subquery), never as a
//     breakdown.
//   - KPI tiles are TRENDS with an additive metric (TOTAL/SUM): the FE's KPI
//     renderer sums the trend series into one number, so TOTAL/SUM render
//     exactly (MAX / UNIQUE_USERS would not). SEGMENTATION is the natural
//     single-scalar fit, but the dashboard FE renders only trends/funnel/
//     retention/top-K — not segmentation results — so it is avoided here.
//     "Metric by dimension, no time axis" is a top-K tile.
//   - currency is always "USD" in the seed, so it is never used as a filter.
// ---------------------------------------------------------------------------

// enum shorthands — keep the tile definitions readable. Each is a typed proto
// enum constant; `.Enum()` returns the pointer the (edition-2023) generated
// structs want.
const (
	srcAuto    = commonv1.PropertySource_PROPERTY_SOURCE_AUTO
	srcCustom  = commonv1.PropertySource_PROPERTY_SOURCE_CUSTOM
	srcProfile = commonv1.PropertySource_PROPERTY_SOURCE_PROFILE

	// Operators used by the tiles below. The engine supports the full set in
	// common.v1.FilterOperator (incl. CONTAINS, LT/LTE, NOT_IN, NOT_BETWEEN);
	// only the ones a tile actually uses are aliased here.
	opEquals      = commonv1.FilterOperator_FILTER_OPERATOR_EQUALS
	opNotEquals   = commonv1.FilterOperator_FILTER_OPERATOR_NOT_EQUALS
	opNotContains = commonv1.FilterOperator_FILTER_OPERATOR_NOT_CONTAINS
	opIsSet       = commonv1.FilterOperator_FILTER_OPERATOR_IS_SET
	opIsNotSet    = commonv1.FilterOperator_FILTER_OPERATOR_IS_NOT_SET
	opGTE         = commonv1.FilterOperator_FILTER_OPERATOR_GTE
	opGT          = commonv1.FilterOperator_FILTER_OPERATOR_GT
	opIn          = commonv1.FilterOperator_FILTER_OPERATOR_IN
	opBetween     = commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN

	logAnd = commonv1.LogicalOperator_LOGICAL_OPERATOR_AND
	logOr  = commonv1.LogicalOperator_LOGICAL_OPERATOR_OR

	aggTotal      = insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL
	aggUniqueUser = insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS
	aggPerUserAvg = insightsv1.AggregationType_AGGREGATION_TYPE_PER_USER_AVG
	aggSum        = insightsv1.AggregationType_AGGREGATION_TYPE_SUM
	aggAvg        = insightsv1.AggregationType_AGGREGATION_TYPE_AVG

	itTrends    = insightsv1.InsightType_INSIGHT_TYPE_TRENDS
	itFunnel    = insightsv1.InsightType_INSIGHT_TYPE_FUNNEL
	itRetention = insightsv1.InsightType_INSIGHT_TYPE_RETENTION
	itTopK      = insightsv1.InsightType_INSIGHT_TYPE_TOP_K

	dimProperty  = insightsv1.TopKQuery_DIMENSION_PROPERTY
	dimEventKind = insightsv1.TopKQuery_DIMENSION_EVENT_KIND
	dimUser      = insightsv1.TopKQuery_DIMENSION_USER

	viewLine     = dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE
	viewArea     = dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_AREA
	viewBarGroup = dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_GROUPED
	viewBarStack = dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_STACKED
	viewTable    = dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_TABLE
	viewKPI      = dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_KPI
)

// ---------------------------------------------------------------------------
// Builder helpers
// ---------------------------------------------------------------------------

// pf builds a single-value property filter (equals / not_equals / contains /
// lt / lte / gt / gte / is_set / is_not_set). For the set operators pass value
// "" — the proto rejects a value there.
func pf(property string, op commonv1.FilterOperator, src commonv1.PropertySource, value string) *commonv1.PropertyFilter {
	f := &commonv1.PropertyFilter{
		Property: proto.String(property),
		Operator: op.Enum(),
		Source:   src.Enum(),
	}
	if value != "" {
		f.Value = proto.String(value)
	}
	return f
}

// pfv builds a multi-value property filter (in / not_in / between / not_between).
func pfv(property string, op commonv1.FilterOperator, src commonv1.PropertySource, values ...string) *commonv1.PropertyFilter {
	return &commonv1.PropertyFilter{
		Property: proto.String(property),
		Operator: op.Enum(),
		Source:   src.Enum(),
		Values:   values,
	}
}

// fgroup builds a FilterGroup combining its filters via op (AND/OR).
func fgroup(op commonv1.LogicalOperator, filters ...*commonv1.PropertyFilter) *insightsv1.FilterGroup {
	return &insightsv1.FilterGroup{Operator: op.Enum(), Filters: filters}
}

// evq builds an EventQuery for kind with optional per-event filters and the
// default TOTAL aggregation.
func evq(kind string, filters ...*commonv1.PropertyFilter) *insightsv1.EventQuery {
	return &insightsv1.EventQuery{Event: efilter(kind, filters...)}
}

// evqAgg builds an EventQuery with an explicit aggregation. aggProp is required
// for SUM/AVG/MIN/MAX and ignored (pass "") for TOTAL/UNIQUE_USERS/PER_USER_AVG.
func evqAgg(kind string, agg insightsv1.AggregationType, aggProp string, filters ...*commonv1.PropertyFilter) *insightsv1.EventQuery {
	q := evq(kind, filters...)
	q.Aggregation = agg.Enum()
	if aggProp != "" {
		q.AggregationProperty = proto.String(aggProp)
	}
	return q
}

// efilter builds a common.v1.EventFilter (kind + optional property filters).
// Used for per-event filters and for session/top-K scopes.
func efilter(kind string, filters ...*commonv1.PropertyFilter) *commonv1.EventFilter {
	ef := &commonv1.EventFilter{Filters: filters}
	if kind != "" {
		ef.Kind = proto.String(kind)
	}
	return ef
}

// bd builds the breakdown list from event property names (auto or custom).
func bd(props ...string) []*insightsv1.Breakdown {
	out := make([]*insightsv1.Breakdown, len(props))
	for i, p := range props {
		out[i] = &insightsv1.Breakdown{Property: proto.String(p)}
	}
	return out
}

// ---------------------------------------------------------------------------
// Fine-grid geometry
//
// The dashboard FE renders tiles on a 72-COLUMN grid — one column (and one
// row) ≈ the ~18px visual gap; see app .../dashboards/constants.ts
// `COLS.lg = 72`, NOT 12. The w*/h* values below are CELL spans in that
// 72-col space (a full row is 72). Content tiles are placed with cell(), which
// insets each tile by one `gap` track on its right and bottom — and drops it
// one track below the cell top — so neighbours show a gutter and the first row
// clears the header band. The FE renders with margin=0 and compactType:null,
// so a visible gap is *only* an empty track; without the inset, tiles sit flush.
//
// Heights MUST clear the FE's per-kind floor: an under-tall tile is clamped UP
// (grid.tsx `Math.max(pos.h, minH)`) and then silently overlaps the tile below
// — the "cascading overlap". Floors in rows: insight 15, KPI 9, markdown 9. The
// cell spans sit a track above each floor so the post-inset height still clears
// it. TestDemoDashboardTileLayout pins the bounds, floor, and no-overlap for
// every seeded tile.
// ---------------------------------------------------------------------------
const (
	gap = 1 // gutter between tiles, in fine grid units (~18px)

	wFull     = 72 // full row
	wTwoThird = 48 // 2/3 — wider side of a 2:1 split
	wWide     = 42 // 7/12 — wider side of a 7:5 split
	wHalf     = 36 // half row
	wNarrow   = 30 // 5/12 — narrower side of a 7:5 split
	wThird    = 24 // 1/3 — narrower side of a 2:1 split
	wQuarter  = 18 // 1/4 row

	hHeader = 9  // markdown section header height (placed directly via gridPos, not cell)
	hStd    = 18 // standard insight chart / KPI cell (17 after inset, clears the 15 floor)
	hTall   = 24 // funnel, retention, cohort table, tall KPI, or full-width trend chart
)

// gridPos builds a tile's grid placement on the 72-column fine grid.
func gridPos(x, y, w, h int32) *dashboardsv1.GridPosition {
	return &dashboardsv1.GridPosition{X: proto.Int32(x), Y: proto.Int32(y), W: proto.Int32(w), H: proto.Int32(h)}
}

// cell places a content tile from a clean cell span (wHalf, hStd, …): it insets
// the tile by one `gap` on its right and bottom and drops it one gap below the
// cell top, so adjacent tiles show a gutter and the first content row clears the
// header band. A content tile's y is therefore the cell top (flush under the
// header); cell() adds the gutter row. The header is placed directly via gridPos
// (a banner inset only on the right, to align with the content gutter).
func cell(x, y, w, h int32) *dashboardsv1.GridPosition {
	return gridPos(x, y+gap, w-gap, h-gap)
}

// threshold builds one KPI threshold rule.
func threshold(op dashboardsv1.ThresholdRule_Operator, value float64, tone dashboardsv1.ThresholdRule_Tone) *dashboardsv1.ThresholdRule {
	return &dashboardsv1.ThresholdRule{Operator: op.Enum(), Value: proto.Float64(value), Tone: tone.Enum()}
}

// tileOpt mutates a TilePayload — used for the optional compare/thresholds.
type tileOpt func(*coredashboards.TilePayload)

func withCompare() tileOpt {
	return func(p *coredashboards.TilePayload) {
		p.Compare = dashboardsv1.ComparePeriod_COMPARE_PERIOD_PRIOR
	}
}

func withThresholds(rules ...*dashboardsv1.ThresholdRule) tileOpt {
	return func(p *coredashboards.TilePayload) { p.Thresholds = rules }
}

// insightTile builds an insight tile from a fully-formed InsightQuerySpec.
func insightTile(name, desc string, view dashboardsv1.DashboardTileViewMode, pos *dashboardsv1.GridPosition, spec *insightsv1.InsightQuerySpec, opts ...tileOpt) coredashboards.UpsertTileInput {
	p := coredashboards.TilePayload{
		DisplayName: name,
		Description: desc,
		ViewMode:    view,
		Content:     coredashboards.InsightTile{Spec: spec},
		Position:    pos,
	}
	for _, o := range opts {
		o(&p)
	}
	return coredashboards.UpsertTileInput{Payload: p}
}

// markdownTile builds a markdown header/note tile. Display name is left empty so
// several can share a board without colliding on the per-dashboard display-name
// unique index.
func markdownTile(body string, pos *dashboardsv1.GridPosition) coredashboards.UpsertTileInput {
	return coredashboards.UpsertTileInput{
		Payload: coredashboards.TilePayload{
			Content:  coredashboards.MarkdownTile{Body: body},
			Position: pos,
		},
	}
}

// ---------------------------------------------------------------------------
// Dashboard definitions
// ---------------------------------------------------------------------------

// dashDef is a demo dashboard: its metadata window plus its tiles. The window
// (default_time_range + default_granularity) drives every tile — tiles store
// only *what* they measure.
type dashDef struct {
	displayName string
	description string
	timeRange   commonv1.TimeRangePreset
	granularity insightsv1.Granularity
	tiles       []coredashboards.UpsertTileInput
}

const (
	rangeLast30  = commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_30_DAYS
	rangeLast90  = commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_90_DAYS
	rangeLast180 = commonv1.TimeRangePreset_TIME_RANGE_PRESET_LAST_180_DAYS

	granDay  = insightsv1.Granularity_GRANULARITY_DAY
	granWeek = insightsv1.Granularity_GRANULARITY_WEEK
)

// demoDashboards returns the full set of seeded demo dashboards. Pure data, so
// the validation test can exercise every tile without a database.
func demoDashboards() []dashDef {
	return []dashDef{
		revenueDashboard(),
		acquisitionDashboard(),
		productHealthDashboard(),
		usersCohortsDashboard(),
	}
}

// revenueDashboard — "Revenue & Commerce": numeric aggregations (Sum/Avg/Max),
// nested mixed-source filter groups, a per-step-filtered funnel with conversion
// window + timing, period comparison, and KPI thresholds.
func revenueDashboard() dashDef {
	return dashDef{
		displayName: "Revenue & Commerce",
		description: "Net revenue, AOV, and checkout conversion — mixed auto/custom/profile filters and numeric aggregations.",
		timeRange:   rangeLast90,
		granularity: granDay,
		tiles: []coredashboards.UpsertTileInput{
			markdownTile(
				"## 💰 Revenue & Commerce\n"+
					"Numeric aggregations (Sum / Avg / Max), nested mixed-source filters, and a checkout funnel with per-step filters + timing.",
				gridPos(0, 0, wFull-gap, hHeader),
			),

			// TRENDS · Area · SUM(amount) — net revenue from real humans on
			// non-zero orders. filter group mixes auto ($bot_score) + profile
			// (email) sources; per-event filter on amount. Compared to prior period.
			insightTile(
				"Net Revenue (humans, paid orders)",
				"SUM(amount) over purchases with amount>0, excluding bots ($bot_score≥50) and internal accounts (email not @pug.sh).",
				viewArea, cell(0, 9, wTwoThird, hStd),
				&insightsv1.InsightQuerySpec{
					InsightType: itTrends.Enum(),
					Events: []*insightsv1.EventQuery{
						evqAgg("purchase", aggSum, "amount", pf("amount", opGT, srcCustom, "0")),
					},
					FilterGroups: []*insightsv1.FilterGroup{
						fgroup(logAnd,
							pf("$bot_score", opGTE, srcAuto, "50"),
							pf("email", opNotContains, srcProfile, "@pug.sh"),
						),
					},
					FilterGroupsOperator: logAnd.Enum(),
				},
				withCompare(),
			),

			// TRENDS · KPI · TOTAL(purchase) — order count with color thresholds. A
			// KPI sums its trend series, so an additive metric (TOTAL) stays exact.
			insightTile(
				"Orders (90d)",
				"Total purchases in the window. Thresholds color the value green above 1k, red below 200.",
				viewKPI, cell(48, 9, wThird, hStd),
				&insightsv1.InsightQuerySpec{
					InsightType: itTrends.Enum(),
					Events:      []*insightsv1.EventQuery{evq("purchase")},
				},
				withThresholds(
					threshold(dashboardsv1.ThresholdRule_OPERATOR_GTE, 1000, dashboardsv1.ThresholdRule_TONE_GOOD),
					threshold(dashboardsv1.ThresholdRule_OPERATOR_LT, 200, dashboardsv1.ThresholdRule_TONE_BAD),
				),
			),

			// TOP_K · Bar · AVG(amount) by category — "AOV by category". Ranked
			// dimension with a numeric metric; scope restricts to purchases.
			insightTile(
				"Average Order Value by Category",
				"Top categories ranked by AVG(amount) on purchase events (+ an $others bucket).",
				viewBarGroup, cell(0, 27, wHalf, hStd),
				&insightsv1.InsightQuerySpec{
					InsightType: itTopK.Enum(),
					TopK: &insightsv1.TopKQuery{
						Dimension:      dimProperty.Enum(),
						Property:       proto.String("category"),
						Metric:         aggAvg.Enum(),
						MetricProperty: proto.String("amount"),
						Scope:          efilter("purchase"),
						Limit:          proto.Int32(6),
					},
				},
			),

			// TOP_K · Table · SUM(amount) by brand — revenue leaderboard.
			insightTile(
				"Revenue by Brand",
				"Brands ranked by SUM(amount) on purchases.",
				viewTable, cell(36, 27, wHalf, hStd),
				&insightsv1.InsightQuerySpec{
					InsightType: itTopK.Enum(),
					TopK: &insightsv1.TopKQuery{
						Dimension:      dimProperty.Enum(),
						Property:       proto.String("brand"),
						Metric:         aggSum.Enum(),
						MetricProperty: proto.String("amount"),
						Scope:          efilter("purchase"),
						Limit:          proto.Int32(8),
					},
				},
			),

			// FUNNEL · per-step filter + conversion window + step timing.
			insightTile(
				"High-Value Cart → Purchase",
				"add_to_cart (price≥50) → checkout_started → purchase, within 24h, with per-step conversion timing.",
				viewBarGroup, cell(0, 45, wTwoThird, hTall),
				&insightsv1.InsightQuerySpec{
					InsightType: itFunnel.Enum(),
					Events: []*insightsv1.EventQuery{
						evq("add_to_cart", pf("price", opGTE, srcCustom, "50")),
						evq("checkout_started"),
						evq("purchase"),
					},
					ConversionWindow:  durationpb.New(24 * time.Hour),
					IncludeStepTiming: proto.Bool(true),
				},
			),

			// TRENDS · KPI · SUM(amount) — gross revenue (all orders, unfiltered).
			// A KPI sums its trend series, so SUM is exact; MAX is not, and
			// SEGMENTATION (the natural MAX fit) isn't rendered by the dashboard FE.
			insightTile(
				"Gross Revenue (90d)",
				"SUM(amount) across all purchases in the window — unfiltered gross, vs the filtered net above.",
				viewKPI, cell(48, 45, wThird, hTall),
				&insightsv1.InsightQuerySpec{
					InsightType: itTrends.Enum(),
					Events:      []*insightsv1.EventQuery{evqAgg("purchase", aggSum, "amount")},
				},
			),

			// TRENDS · Line · SUM(discount_amount) — coupon spend, vs prior period.
			insightTile(
				"Coupon Discount Spend",
				"SUM(discount_amount) on coupon_applied events, compared to the prior period.",
				viewLine, cell(0, 69, wFull, hStd),
				&insightsv1.InsightQuerySpec{
					InsightType: itTrends.Enum(),
					Events:      []*insightsv1.EventQuery{evqAgg("coupon_applied", aggSum, "discount_amount")},
				},
				withCompare(),
			),
		},
	}
}

// acquisitionDashboard — "Acquisition & Marketing": UTM attribution with the
// IN / IS SET / IS NOT SET / NOT CONTAINS operators, a breakdown funnel, top-K
// rankings of marketing dimensions, and traffic/conversions by source & platform.
func acquisitionDashboard() dashDef {
	return dashDef{
		displayName: "Acquisition & Marketing",
		description: "Channel attribution, a paid-traffic purchase funnel, and traffic/conversions by source — UTM/referrer filters and breakdowns.",
		timeRange:   rangeLast90,
		granularity: granDay,
		tiles: []coredashboards.UpsertTileInput{
			markdownTile(
				"## 📣 Acquisition & Marketing\n"+
					"UTM/referrer attribution, a paid-channel browse→buy funnel, top-K campaign revenue, and traffic/conversions by source.",
				gridPos(0, 0, wFull-gap, hHeader),
			),

			// FUNNEL · 3-step browse→buy funnel, broken down by channel and gated to
			// paid mediums (IN operator). Built from commerce events that branch
			// across journeys (bounce/browse stop at product_viewed; only purchase
			// journeys reach add_to_cart/purchase) so the funnel shows real drop-off.
			// Deliberately NOT page_view→signup: page_view is universal while signup
			// is a once-per-user first-session event, which collapses that funnel to a
			// near-empty <1% second step at any dataset size.
			insightTile(
				"Paid Traffic: Browse → Buy by Medium",
				"product_viewed → add_to_cart → purchase, restricted to paid mediums ($utmMedium IN cpc/paid_social/email) and split by medium.",
				viewBarGroup, cell(0, 9, wWide, hTall),
				&insightsv1.InsightQuerySpec{
					InsightType: itFunnel.Enum(),
					Events:      []*insightsv1.EventQuery{evq("product_viewed"), evq("add_to_cart"), evq("purchase")},
					Breakdowns:  bd("$utmMedium"),
					FilterGroups: []*insightsv1.FilterGroup{
						fgroup(logAnd, pfv("$utmMedium", opIn, srcAuto, "cpc", "paid_social", "email")),
					},
				},
			),

			// TRENDS · KPI · TOTAL(signup) for organic/direct (IS NOT SET). A KPI
			// sums its series; signup is once per user, so the count is exact.
			insightTile(
				"Organic Signups (90d)",
				"Signups with no paid source ($utmSource is not set).",
				viewKPI, cell(42, 9, wNarrow, hTall),
				&insightsv1.InsightQuerySpec{
					InsightType: itTrends.Enum(),
					Events:      []*insightsv1.EventQuery{evq("signup")},
					FilterGroups: []*insightsv1.FilterGroup{
						fgroup(logAnd, pf("$utmSource", opIsNotSet, srcAuto, "")),
					},
				},
			),

			// TOP_K · Table · SUM(amount) by campaign.
			insightTile(
				"Revenue by Campaign",
				"Campaigns ranked by SUM(amount) on attributed purchases.",
				viewTable, cell(0, 33, wHalf, hStd),
				&insightsv1.InsightQuerySpec{
					InsightType: itTopK.Enum(),
					TopK: &insightsv1.TopKQuery{
						Dimension:      dimProperty.Enum(),
						Property:       proto.String("$utmCampaign"),
						Metric:         aggSum.Enum(),
						MetricProperty: proto.String("amount"),
						Scope:          efilter("purchase"),
						Limit:          proto.Int32(8),
					},
				},
			),

			// TOP_K · Bar · referrer ranking with IS SET + NOT CONTAINS scope.
			insightTile(
				"Top Referrers (non-direct, non-Google)",
				"Referrers ranked by page views, excluding direct ($referrer is set) and Google ($referrer not contains 'google').",
				viewBarGroup, cell(36, 33, wHalf, hStd),
				&insightsv1.InsightQuerySpec{
					InsightType: itTopK.Enum(),
					TopK: &insightsv1.TopKQuery{
						Dimension: dimProperty.Enum(),
						Property:  proto.String("$referrer"),
						Scope: efilter("page_view",
							pf("$referrer", opIsSet, srcAuto, ""),
							pf("$referrer", opNotContains, srcAuto, "google"),
						),
						Limit: proto.Int32(8),
					},
				},
			),

			// TRENDS · Area · TOTAL(page_view) stacked by acquisition source, with a
			// known source (IS SET) — drops the dominant direct/organic bucket so the
			// paid/referral channels are actually comparable.
			insightTile(
				"Traffic by Source",
				"Page views over time, stacked by acquisition source ($utmSource is set — direct/organic excluded).",
				viewArea, cell(0, 51, wFull, hTall),
				&insightsv1.InsightQuerySpec{
					InsightType:    itTrends.Enum(),
					Events:         []*insightsv1.EventQuery{evq("page_view")},
					Breakdowns:     bd("$utmSource"),
					BreakdownLimit: proto.Int32(8),
					FilterGroups: []*insightsv1.FilterGroup{
						fgroup(logAnd, pf("$utmSource", opIsSet, srcAuto, "")),
					},
				},
			),

			// TRENDS · Bar · TOTAL(purchase) on web with a known source (EQUALS +
			// IS SET filters), by source — direct/organic dropped so the channels
			// are comparable.
			insightTile(
				"Web Purchases by Source",
				"Purchases on web ($platform = web) with a known source ($utmSource is set), broken down by acquisition source.",
				viewBarGroup, cell(0, 75, wFull, hTall),
				&insightsv1.InsightQuerySpec{
					InsightType: itTrends.Enum(),
					Events:      []*insightsv1.EventQuery{evq("purchase")},
					Breakdowns:  bd("$utmSource"),
					FilterGroups: []*insightsv1.FilterGroup{
						fgroup(logAnd,
							pf("$platform", opEquals, srcAuto, "web"),
							pf("$utmSource", opIsSet, srcAuto, ""),
						),
					},
				},
			),

			// TRENDS · Line · UNIQUE_USERS(page_view) — daily active users by platform.
			insightTile(
				"Active Users by Platform",
				"Daily active users (distinct) split by $platform (web / ios / android).",
				viewLine, cell(0, 99, wFull, hStd),
				&insightsv1.InsightQuerySpec{
					InsightType: itTrends.Enum(),
					Events:      []*insightsv1.EventQuery{evqAgg("page_view", aggUniqueUser, "")},
					Breakdowns:  bd("$platform"),
				},
			),
		},
	}
}

// productHealthDashboard — "Product & UX Health": top-K over property / event-kind
// dimensions, top pages and error codes, multi-breakdown stacked errors, a
// unique-user crash view, and a per-user-average trend.
func productHealthDashboard() dashDef {
	return dashDef{
		displayName: "Product & UX Health",
		description: "Search, navigation, errors, and crashes — top-K rankings, top pages/error codes, and multi-dimension breakdowns.",
		timeRange:   rangeLast30,
		granularity: granDay,
		tiles: []coredashboards.UpsertTileInput{
			markdownTile(
				"## 🛠 Product & UX Health\n"+
					"Top products, pages & events, error-code rankings, stacked error breakdowns, and crash-by-version.",
				gridPos(0, 0, wFull-gap, hHeader),
			),

			// TOP_K · Table · most-viewed products.
			insightTile(
				"Top Products Viewed",
				"Products ranked by product_viewed count (+ an $others bucket).",
				viewTable, cell(0, 9, wHalf, hStd),
				&insightsv1.InsightQuerySpec{
					InsightType: itTopK.Enum(),
					TopK: &insightsv1.TopKQuery{
						Dimension: dimProperty.Enum(),
						Property:  proto.String("product_name"),
						Scope:     efilter("product_viewed"),
						Limit:     proto.Int32(10),
					},
				},
			),

			// TOP_K · Bar · EVENT_KIND dimension, scoped to the native app (NOT_EQUALS).
			insightTile(
				"Top Events (native app)",
				"Most frequent event kinds on the native app ($platform != web).",
				viewBarGroup, cell(36, 9, wHalf, hStd),
				&insightsv1.InsightQuerySpec{
					InsightType: itTopK.Enum(),
					TopK: &insightsv1.TopKQuery{
						Dimension: dimEventKind.Enum(),
						Scope:     efilter("", pf("$platform", opNotEquals, srcAuto, "web")),
						Limit:     proto.Int32(12),
					},
				},
			),

			// TOP_K · Table · most-viewed pages ($url on page_view).
			insightTile(
				"Top Pages",
				"Pages ranked by page_view count (+ an $others bucket).",
				viewTable, cell(0, 27, wHalf, hStd),
				&insightsv1.InsightQuerySpec{
					InsightType: itTopK.Enum(),
					TopK: &insightsv1.TopKQuery{
						Dimension: dimProperty.Enum(),
						Property:  proto.String("$url"),
						Scope:     efilter("page_view"),
						Limit:     proto.Int32(10),
					},
				},
			),

			// TOP_K · Table · most common error codes (error_code on error_occurred).
			insightTile(
				"Top Error Codes",
				"Error codes ranked by error_occurred count (+ an $others bucket).",
				viewTable, cell(36, 27, wHalf, hStd),
				&insightsv1.InsightQuerySpec{
					InsightType: itTopK.Enum(),
					TopK: &insightsv1.TopKQuery{
						Dimension: dimProperty.Enum(),
						Property:  proto.String("error_code"),
						Scope:     efilter("error_occurred"),
						Limit:     proto.Int32(10),
					},
				},
			),

			// TRENDS · Stacked bar · two breakdowns (severity × platform).
			insightTile(
				"Errors by Severity & Platform",
				"error_occurred volume split by both severity and $platform (two-dimension stacked breakdown).",
				viewBarStack, cell(0, 45, wWide, hStd),
				&insightsv1.InsightQuerySpec{
					InsightType: itTrends.Enum(),
					Events:      []*insightsv1.EventQuery{evq("error_occurred")},
					Breakdowns:  bd("severity", "$platform"),
				},
			),

			// TRENDS · Bar · UNIQUE_USERS affected by crashes, by app version.
			insightTile(
				"Crashing Users by App Version",
				"Distinct users hitting app_crashed, broken down by $app_version (native app only).",
				viewBarGroup, cell(42, 45, wNarrow, hStd),
				&insightsv1.InsightQuerySpec{
					InsightType: itTrends.Enum(),
					Events:      []*insightsv1.EventQuery{evqAgg("app_crashed", aggUniqueUser, "")},
					Breakdowns:  bd("$app_version"),
					FilterGroups: []*insightsv1.FilterGroup{
						fgroup(logAnd, pfv("$platform", opIn, srcAuto, "ios", "android")),
					},
				},
			),

			// TRENDS · Line · PER_USER_AVG product views.
			insightTile(
				"Avg Product Views per User",
				"PER_USER_AVG of product_viewed — browsing depth per active user.",
				viewLine, cell(0, 63, wFull, hStd),
				&insightsv1.InsightQuerySpec{
					InsightType: itTrends.Enum(),
					Events:      []*insightsv1.EventQuery{evqAgg("product_viewed", aggPerUserAvg, "")},
				},
			),
		},
	}
}

// usersCohortsDashboard — "Users, Cohorts & Segments": retention cohorts (with a
// distinct return event and a breakdown), a USER-dimension top-K with profile
// enrichment, and profile-filtered segments (IS SET / BETWEEN on profile props).
func usersCohortsDashboard() dashDef {
	return dashDef{
		displayName: "Users, Cohorts & Segments",
		description: "Retention, whale users, and profile-filtered segments — cohorts, USER top-K, and PROFILE-source filters.",
		timeRange:   rangeLast180,
		granularity: granWeek,
		tiles: []coredashboards.UpsertTileInput{
			markdownTile(
				"## 🐾 Users, Cohorts & Segments\n"+
					"Signup→purchase retention, app retention by OS, top customers (with dog-profile enrichment), and profile-filtered segments.",
				gridPos(0, 0, wFull-gap, hHeader),
			),

			// RETENTION · Table · distinct start (signup) and return (purchase) events.
			insightTile(
				"Signup → Repeat Purchase Retention",
				"Weekly cohorts of new signups, retained by whether they later purchase.",
				viewTable, cell(0, 9, wFull, hTall),
				&insightsv1.InsightQuerySpec{
					InsightType: itRetention.Enum(),
					Events:      []*insightsv1.EventQuery{evq("signup"), evq("purchase")},
				},
			),

			// RETENTION · Line · same-event retention with a breakdown (+ limit).
			insightTile(
				"App Open Retention by OS",
				"Weekly app_open retention cohorts, split by $os (top 4 values).",
				viewLine, cell(0, 33, wFull, hStd),
				&insightsv1.InsightQuerySpec{
					InsightType:    itRetention.Enum(),
					Events:         []*insightsv1.EventQuery{evq("app_open"), evq("app_open")},
					Breakdowns:     bd("$os"),
					BreakdownLimit: proto.Int32(4),
				},
			),

			// TOP_K · Table · USER dimension — rows carry dog-profile enrichment.
			insightTile(
				"Top Customers (whales)",
				"Users ranked by purchase count. USER-dimension rows are enriched with the dog's profile (name, breed, city).",
				viewTable, cell(0, 51, wNarrow, hTall),
				&insightsv1.InsightQuerySpec{
					InsightType: itTopK.Enum(),
					TopK: &insightsv1.TopKQuery{
						Dimension: dimUser.Enum(),
						Metric:    aggTotal.Enum(),
						Scope:     efilter("purchase"),
						Limit:     proto.Int32(10),
					},
				},
			),

			// TRENDS · KPI · TOTAL(purchase) over a nested OR group mixing a profile
			// prop with an order amount — a "VIP" order = club member OR big spender.
			// Distinct VIP *customers* needs segmentation (not rendered by the FE);
			// a KPI sums its series, so we count VIP orders instead.
			insightTile(
				"VIP Orders (90d)",
				"Purchases that are VIP — an OR group mixing a profile prop (pug_club is set) with a custom prop (amount ≥ 100).",
				viewKPI, cell(30, 51, wQuarter, hTall),
				&insightsv1.InsightQuerySpec{
					InsightType: itTrends.Enum(),
					Events:      []*insightsv1.EventQuery{evq("purchase")},
					FilterGroups: []*insightsv1.FilterGroup{
						fgroup(logOr,
							pf("pug_club", opIsSet, srcProfile, ""),
							pf("amount", opGTE, srcCustom, "100"),
						),
					},
				},
			),

			// TRENDS · Bar · profile BETWEEN filter + auto breakdown.
			insightTile(
				"Active Senior Dogs by Country",
				"Distinct active users whose dog is a senior (age_years BETWEEN 8 and 12, a profile prop), broken down by $country.",
				viewBarGroup, cell(48, 51, wThird, hTall),
				&insightsv1.InsightQuerySpec{
					InsightType:    itTrends.Enum(),
					Events:         []*insightsv1.EventQuery{evqAgg("page_view", aggUniqueUser, "")},
					Breakdowns:     bd("$country"),
					BreakdownLimit: proto.Int32(8),
					FilterGroups: []*insightsv1.FilterGroup{
						fgroup(logAnd, pfv("age_years", opBetween, srcProfile, "8", "12")),
					},
				},
			),
		},
	}
}

// ---------------------------------------------------------------------------
// Seeding
// ---------------------------------------------------------------------------

// SeedDemoDashboards idempotently ensures the demo project's showcase dashboards
// exist. It is safe to call on every seed run / worker start: a dashboard is
// (re)created only when no dashboard with its display name is present, so a full
// reset recreates them and a warm restart leaves them untouched. Dashboards are
// static config (they reference event kinds/properties, not specific rows), so
// they need no clearing on the --no-reset path.
func SeedDemoDashboards(ctx context.Context, pg *pgxpool.Pool, projectID string) error {
	// A one-shot seeding context: reads on the writer pool are fine, so the same
	// pool backs both the read and write handles.
	svc := coredashboards.NewService(pg, pg)

	existing, err := svc.ListDashboards(ctx, projectID)
	if err != nil {
		return err
	}
	have := make(map[string]struct{}, len(existing))
	for _, d := range existing {
		have[d.Dashboard.DisplayName] = struct{}{}
	}

	for _, def := range demoDashboards() {
		if _, ok := have[def.displayName]; ok {
			continue
		}
		created, err := svc.CreateDashboard(ctx, projectID, def.displayName, def.description, def.timeRange, def.granularity)
		if err != nil {
			return err
		}
		if _, err := svc.UpsertDashboard(ctx, projectID, created.ID, coredashboards.UpsertDashboardInput{
			DisplayName:        def.displayName,
			Description:        def.description,
			DefaultTimeRange:   def.timeRange,
			DefaultGranularity: def.granularity,
			Tiles:              def.tiles,
		}); err != nil {
			return err
		}
		slog.InfoContext(ctx, "seeded demo dashboard",
			slog.String("project_id", projectID),
			slog.String("dashboard", def.displayName),
			slog.Int("tiles", len(def.tiles)),
		)
	}
	return nil
}
