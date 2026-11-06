package seed

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/pug-sh/pug/internal/attribution"
	coredashboards "github.com/pug-sh/pug/internal/core/dashboards"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
)

// The demo boards, as designed in docs/architecture/demo.md. Every tile reads the
// seeded Pug & Pals vocabulary (internal/app/seed/clickhouse) and uses only views
// the dashboard FE renders.

const (
	srcAuto    = commonv1.PropertySource_PROPERTY_SOURCE_AUTO
	srcCustom  = commonv1.PropertySource_PROPERTY_SOURCE_CUSTOM
	srcProfile = commonv1.PropertySource_PROPERTY_SOURCE_PROFILE

	opEquals  = commonv1.FilterOperator_FILTER_OPERATOR_EQUALS
	opIsSet   = commonv1.FilterOperator_FILTER_OPERATOR_IS_SET
	opIn      = commonv1.FilterOperator_FILTER_OPERATOR_IN
	opBetween = commonv1.FilterOperator_FILTER_OPERATOR_BETWEEN

	logAnd = commonv1.LogicalOperator_LOGICAL_OPERATOR_AND

	aggTotal      = insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL
	aggUniqueUser = insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS
	aggSum        = insightsv1.AggregationType_AGGREGATION_TYPE_SUM
	aggAvg        = insightsv1.AggregationType_AGGREGATION_TYPE_AVG

	itTrends    = insightsv1.InsightType_INSIGHT_TYPE_TRENDS
	itFunnel    = insightsv1.InsightType_INSIGHT_TYPE_FUNNEL
	itRetention = insightsv1.InsightType_INSIGHT_TYPE_RETENTION
	itUserFlow  = insightsv1.InsightType_INSIGHT_TYPE_USER_FLOW
	itTopK      = insightsv1.InsightType_INSIGHT_TYPE_TOP_K
	itMap       = insightsv1.InsightType_INSIGHT_TYPE_MAP

	dimProperty = insightsv1.TopKQuery_DIMENSION_PROPERTY
	dimUser     = insightsv1.TopKQuery_DIMENSION_USER

	viewLine     = dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE
	viewArea     = dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_AREA
	viewBarGroup = dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_GROUPED
	viewBarStack = dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_STACKED
	viewTable    = dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_TABLE
	viewKPI      = dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_KPI
	viewSankey   = dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_SANKEY
	viewPie      = dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_PIE
	viewMap      = dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_MAP

	formatCompact  = dashboardsv1.VisualizationOptions_Y_AXIS_FORMAT_COMPACT
	formatDuration = dashboardsv1.VisualizationOptions_Y_AXIS_FORMAT_DURATION_MS
)

var paidChannels = []string{attribution.ChannelPaidSearch, attribution.ChannelPaidSocial}

// pf builds a single-value property filter. Pass value "" for the set
// operators: the proto rejects a value there.
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

func efilter(kind string, filters ...*commonv1.PropertyFilter) *commonv1.EventFilter {
	ef := &commonv1.EventFilter{Filters: filters}
	if kind != "" {
		ef.Kind = proto.String(kind)
	}
	return ef
}

func evq(kind string, filters ...*commonv1.PropertyFilter) *insightsv1.EventQuery {
	return &insightsv1.EventQuery{Event: efilter(kind, filters...)}
}

// evqAgg sets an explicit aggregation. aggProp is required for SUM/AVG/MIN/MAX
// and ignored ("") otherwise.
func evqAgg(kind string, agg insightsv1.AggregationType, aggProp string) *insightsv1.EventQuery {
	q := evq(kind)
	q.Aggregation = agg.Enum()
	if aggProp != "" {
		q.AggregationProperty = proto.String(aggProp)
	}
	return q
}

func trend(events ...*insightsv1.EventQuery) *insightsv1.InsightQuerySpec {
	return &insightsv1.InsightQuerySpec{InsightType: itTrends.Enum(), Events: events}
}

// split breaks a spec down by one event property. A limit of 0 keeps the
// server default.
func split(spec *insightsv1.InsightQuerySpec, property string, limit int32) *insightsv1.InsightQuerySpec {
	spec.Breakdowns = []*insightsv1.Breakdown{{Property: proto.String(property)}}
	if limit > 0 {
		spec.BreakdownLimit = proto.Int32(limit)
	}
	return spec
}

// where ANDs filters onto the whole spec.
func where(spec *insightsv1.InsightQuerySpec, filters ...*commonv1.PropertyFilter) *insightsv1.InsightQuerySpec {
	spec.FilterGroups = append(spec.FilterGroups, &insightsv1.FilterGroup{Operator: logAnd.Enum(), Filters: filters})
	spec.FilterGroupsOperator = logAnd.Enum()
	return spec
}

// withBots turns on the include-bots toggle, which every insight leaves off by
// default.
func withBots(spec *insightsv1.InsightQuerySpec) *insightsv1.InsightQuerySpec {
	spec.IncludeBots = proto.Bool(true)
	return spec
}

func funnel(window time.Duration, stepTiming bool, steps ...*insightsv1.EventQuery) *insightsv1.InsightQuerySpec {
	spec := &insightsv1.InsightQuerySpec{
		InsightType:      itFunnel.Enum(),
		Events:           steps,
		ConversionWindow: durationpb.New(window),
	}
	if stepTiming {
		spec.IncludeStepTiming = proto.Bool(true)
	}
	return spec
}

// ranked is a top K over a property. metricProp is "" for count metrics.
func ranked(property string, metric insightsv1.AggregationType, metricProp string, scope *commonv1.EventFilter, limit int32, others bool) *insightsv1.InsightQuerySpec {
	q := &insightsv1.TopKQuery{
		Dimension:  dimProperty.Enum(),
		Property:   proto.String(property),
		Metric:     metric.Enum(),
		Scope:      scope,
		Limit:      proto.Int32(limit),
		OmitOthers: proto.Bool(!others),
	}
	if metricProp != "" {
		q.MetricProperty = proto.String(metricProp)
	}
	return &insightsv1.InsightQuerySpec{InsightType: itTopK.Enum(), TopK: q}
}

func countryMap(scope *commonv1.EventFilter, metric insightsv1.AggregationType, metricProp string) *insightsv1.InsightQuerySpec {
	q := &insightsv1.MapQuery{Scope: scope, Metric: metric.Enum()}
	if metricProp != "" {
		q.MetricProperty = proto.String(metricProp)
	}
	return &insightsv1.InsightQuerySpec{InsightType: itMap.Enum(), Map: q}
}

// The FE grid is 72 columns of ~18px rows with no margin, so cell() leaves one
// empty column as a gutter. A height under the FE's per-kind floor is clamped up
// and overlaps the tile below; TestDemoDashboardTileLayout pins both.
const (
	gap = 1

	wFull     = 72
	wTwoThird = 48
	wWide     = 42
	wHalf     = 36
	wNarrow   = 30
	wThird    = 24
	wQuarter  = 18

	hKPI     = 16
	hRanked  = 19 // a ranked list of about 6 rows
	hStd     = 22
	hList    = 24 // a ranked list of about 10 rows
	hLarge   = 26 // hero row, explainer, map
	hSankey  = 32
	hUsers   = 36 // ten users, each with a two-line profile
	hCohort  = 40
	hSplitFn = 51 // a three-way split funnel in the FE's two-wide grid, plus its series list
)

func cell(x, y, w, h int32) *dashboardsv1.GridPosition {
	return &dashboardsv1.GridPosition{X: proto.Int32(x), Y: proto.Int32(y), W: proto.Int32(w - gap), H: proto.Int32(h)}
}

func threshold(op dashboardsv1.ThresholdRule_Operator, value float64, tone dashboardsv1.ThresholdRule_Tone) *dashboardsv1.ThresholdRule {
	return &dashboardsv1.ThresholdRule{Operator: op.Enum(), Value: proto.Float64(value), Tone: tone.Enum()}
}

type tileOpt func(*coredashboards.TilePayload)

func withCompare() tileOpt {
	return func(p *coredashboards.TilePayload) {
		p.Compare = dashboardsv1.ComparePeriod_COMPARE_PERIOD_PRIOR
	}
}

func withThresholds(rules ...*dashboardsv1.ThresholdRule) tileOpt {
	return func(p *coredashboards.TilePayload) { p.Thresholds = rules }
}

func withFormat(format dashboardsv1.VisualizationOptions_YAxisFormat) tileOpt {
	return func(p *coredashboards.TilePayload) {
		p.Visualization = &dashboardsv1.VisualizationOptions{YAxisFormat: format.Enum()}
	}
}

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

// markdownTile leaves the display name empty: empty names are exempt from the
// per-dashboard unique index.
func markdownTile(body string, pos *dashboardsv1.GridPosition) coredashboards.UpsertTileInput {
	return coredashboards.UpsertTileInput{
		Payload: coredashboards.TilePayload{
			Content:  coredashboards.MarkdownTile{Body: body},
			Position: pos,
		},
	}
}

// dashDef is a demo dashboard. Its window drives every tile; tiles store only
// what they measure.
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

// demoDashboards returns the seeded boards in list order (boards list by
// creation time). Pure data, so the tests exercise every tile without a database.
func demoDashboards() []dashDef {
	return []dashDef{
		storeDashboard(),
		marketingDashboard(),
		journeysDashboard(),
		loyaltyDashboard(),
		appDashboard(),
	}
}

func storeDashboard() dashDef {
	return dashDef{
		displayName: "Store at a glance",
		description: "How the shop is doing this month: revenue, orders and customers, with the change since last month.",
		timeRange:   rangeLast30,
		granularity: granDay,
		tiles: []coredashboards.UpsertTileInput{
			markdownTile(
				"### 🛍️ Store at a glance\n\n"+
					"The numbers a shop checks every morning, with what changed since last month.\n\n"+
					"- **KPIs with deltas** against the previous 30 days\n"+
					"- **Revenue by category**, day by day\n"+
					"- **A revenue map** of where customers buy\n"+
					"- **Best sellers**, with the long tail rolled into Others\n\n"+
					"Every number comes straight from raw events in ClickHouse. There is no warehouse to build or sync.\n",
				cell(0, 0, wThird, hLarge),
			),
			insightTile("Revenue by category", "Sum of order amounts, split by product category.",
				viewBarStack, cell(wThird, 0, wTwoThird, hLarge),
				split(trend(evqAgg("purchase", aggSum, "amount")), "category", 4),
				withFormat(formatCompact)),

			insightTile("Revenue", "Sum of order amounts, against the previous 30 days.",
				viewKPI, cell(0, 26, wQuarter, hKPI),
				trend(evqAgg("purchase", aggSum, "amount")),
				withCompare(), withFormat(formatCompact)),
			insightTile("Orders", "Count of purchases, against the previous 30 days.",
				viewKPI, cell(wQuarter, 26, wQuarter, hKPI),
				trend(evq("purchase")),
				withCompare()),
			insightTile("Average order value", "Average order amount. Green once it clears $35.",
				viewKPI, cell(2*wQuarter, 26, wQuarter, hKPI),
				trend(evqAgg("purchase", aggAvg, "amount")),
				withCompare(),
				withThresholds(threshold(dashboardsv1.ThresholdRule_OPERATOR_GTE, 35, dashboardsv1.ThresholdRule_TONE_GOOD))),
			insightTile("New customers", "Count of signups, against the previous 30 days.",
				viewKPI, cell(3*wQuarter, 26, wQuarter, hKPI),
				trend(evq("signup")),
				withCompare()),

			insightTile("Revenue by country", "A map of order revenue by the buyer's country.",
				viewMap, cell(0, 42, wWide, hLarge),
				countryMap(efilter("purchase"), aggSum, "amount")),
			insightTile("Orders by platform", "Purchases on web, iOS and Android.",
				viewPie, cell(wWide, 42, wNarrow, hLarge),
				split(trend(evq("purchase")), "$platform", 3)),

			insightTile("Visitors per day", "Unique visitors on the web store.",
				viewArea, cell(0, 68, wHalf, hList),
				trend(evqAgg("page_view", aggUniqueUser, ""))),
			insightTile("Best sellers", "Products ranked by revenue, with the rest rolled into Others.",
				viewTable, cell(wHalf, 68, wHalf, hList),
				ranked("product_name", aggSum, "amount", efilter("purchase"), 8, true)),

			insightTile("Average order by platform", "Average order amount on each platform.",
				viewLine, cell(0, 92, wFull, hStd),
				split(trend(evqAgg("purchase", aggAvg, "amount")), "$platform", 3)),
		},
	}
}

func marketingDashboard() dashDef {
	return dashDef{
		displayName: "Marketing & channels",
		description: "Where customers come from and which channels pay: channel attribution, campaigns and crawler traffic.",
		timeRange:   rangeLast90,
		granularity: granDay,
		tiles: []coredashboards.UpsertTileInput{
			markdownTile(
				"### 📣 Marketing & channels\n\n"+
					"See where visitors come from and which channels turn them into customers.\n\n"+
					"- **Channels** classified automatically from UTM tags and referrers\n"+
					"- **Conversion by channel**, as funnels side by side\n"+
					"- **Campaign revenue** and a map of paid reach\n"+
					"- **Crawlers and AI bots**, tagged and kept out of your numbers\n\n"+
					"Pug works out attribution and tags bots at ingest, with no extra code.\n",
				cell(0, 0, wThird, hLarge),
			),
			insightTile("Visitors by channel", "Unique visitors by the channel Pug assigns at ingest.",
				viewLine, cell(wThird, 0, wTwoThird, hLarge),
				split(trend(evqAgg("page_view", aggUniqueUser, "")), "$channel", 4)),

			insightTile("Which channels convert", "Visit, add to cart, purchase within a day, split by channel.",
				viewBarGroup, cell(0, 26, wFull, hSplitFn),
				where(
					split(funnel(24*time.Hour, false, evq("page_view"), evq("add_to_cart"), evq("purchase")), "$channel", 0),
					pfv("$channel", opIn, srcAuto, attribution.ChannelEmail, attribution.ChannelPaidSearch, attribution.ChannelPaidSocial),
				)),

			insightTile("Where paid visitors come from", "Unique visitors from paid channels, by country.",
				viewMap, cell(0, 77, wWide, hLarge),
				countryMap(efilter("page_view", pfv("$channel", opIn, srcAuto, paidChannels...)), aggUniqueUser, "")),
			insightTile("Newsletter signups", "Submissions of the Pupdates form.",
				viewArea, cell(wWide, 77, wNarrow, hLarge),
				trend(evq("form_submit", pf("form_name", opEquals, srcCustom, "pupdates")))),

			insightTile("Signups from paid channels", "Signups from paid search and paid social.",
				viewKPI, cell(0, 103, wThird, hRanked),
				where(trend(evq("signup")), pfv("$channel", opIn, srcAuto, paidChannels...)),
				withCompare()),
			insightTile("Revenue by campaign", "Campaigns ranked by the revenue of their sessions.",
				viewTable, cell(wThird, 103, wTwoThird, hRanked),
				ranked("$utmCampaign", aggSum, "amount", efilter("purchase", pf("$utmCampaign", opIsSet, srcAuto, "")), 6, false)),

			insightTile("Crawler visits, by bot", "Page views Pug tagged as bots, shown with include bots on.",
				viewBarStack, cell(0, 122, wFull, hLarge),
				withBots(where(split(trend(evq("page_view")), "$bot_reason", 4), pf("$bot", opEquals, srcAuto, "true")))),
		},
	}
}

func journeysDashboard() dashDef {
	return dashDef{
		displayName: "Shopper journeys",
		description: "How people move through the store and where they drop off: paths, the search funnel and frustration signals.",
		timeRange:   rangeLast30,
		granularity: granDay,
		tiles: []coredashboards.UpsertTileInput{
			// The Sankey leads at full width: five columns need about 170px each and
			// scroll sideways at two-thirds width.
			insightTile("How shoppers move", "A user flow over page types, from the landing page toward checkout.",
				viewSankey, cell(0, 0, wFull, hSankey),
				&insightsv1.InsightQuerySpec{
					InsightType: itUserFlow.Enum(),
					UserFlow: &insightsv1.UserFlowQuery{
						NodeKind:     insightsv1.UserFlowQuery_NODE_KIND_PROPERTY.Enum(),
						NodeProperty: proto.String("page_type"),
						Scope:        efilter("page_view"),
						MaxHops:      proto.Int32(4),
						MaxNodes:     proto.Int32(10),
					},
				}),

			markdownTile(
				"### 🧭 Shopper journeys\n\n"+
					"Follow real paths through the store and find where shoppers give up.\n\n"+
					"- **A flow of page types**, from landing page to checkout\n"+
					"- **Search to order**, step by step\n"+
					"- **Rage clicks and scroll depth**, page by page\n"+
					"- **What shoppers search for**, straight from the search box\n\n"+
					"The SDK captures page views, clicks and rage clicks on its own.\n",
				cell(0, 32, wThird, hLarge),
			),
			insightTile("Rage clicks", "Bursts of repeated clicks, captured automatically by the SDK.",
				viewArea, cell(wThird, 32, wTwoThird, hLarge),
				trend(evq("rage_click"))),

			insightTile("Search to order", "Visit, search, add to cart, purchase within a day, with the time each step takes.",
				viewBarGroup, cell(0, 58, wTwoThird, hList),
				funnel(24*time.Hour, true, evq("page_view"), evq("search"), evq("add_to_cart"), evq("purchase"))),
			insightTile("Average basket", "Average items per checkout, against the previous 30 days.",
				viewKPI, cell(wTwoThird, 58, wThird, hList),
				trend(evqAgg("checkout_started", aggAvg, "item_count")),
				withCompare()),

			insightTile("Searches per day", "Searches on every platform.",
				viewBarGroup, cell(0, 82, wHalf, hList),
				trend(evq("search"))),
			insightTile("What dogs search for", "Search terms, ranked.",
				viewTable, cell(wHalf, 82, wHalf, hList),
				ranked("query", aggTotal, "", efilter("search"), 10, false)),

			insightTile("How far shoppers scroll", "Average scroll depth on each page type.",
				viewTable, cell(0, 106, wHalf, hStd),
				ranked("page_type", aggAvg, "percent", efilter("scroll", pf("$platform", opEquals, srcAuto, "web")), 10, false)),
			insightTile("How they pay", "Purchases by payment method.",
				viewPie, cell(wHalf, 106, wHalf, hStd),
				split(trend(evq("purchase")), "payment_method", 4)),
		},
	}
}

func loyaltyDashboard() dashDef {
	return dashDef{
		displayName: "Customers & loyalty",
		description: "Who comes back and who pays: retention cohorts, Pug Club, and top customers with their profiles.",
		timeRange:   rangeLast180,
		granularity: granWeek,
		tiles: []coredashboards.UpsertTileInput{
			markdownTile(
				"### 🐾 Customers & loyalty\n\n"+
					"Who comes back, who joins Pug Club, and what keeps them happy.\n\n"+
					"- **Retention cohorts** for every signup week\n"+
					"- **A Club funnel**, from the Club page to a paid plan\n"+
					"- **Members vs everyone else**, week by week\n"+
					"- **Top customers**, each with their dog's profile\n\n"+
					"Anonymous visits join a customer's profile the moment they sign up.\n",
				cell(0, 0, wThird, hLarge),
			),
			insightTile("Weekly shoppers by platform", "Unique shoppers each week on web, iOS and Android.",
				viewBarStack, cell(wThird, 0, wTwoThird, hLarge),
				split(trend(evqAgg("product_viewed", aggUniqueUser, "")), "$platform", 3)),

			insightTile("Do new customers come back?", "Weekly signup cohorts, and how many browse again in later weeks.",
				viewTable, cell(0, 26, wFull, hCohort),
				&insightsv1.InsightQuerySpec{
					InsightType: itRetention.Enum(),
					Events:      []*insightsv1.EventQuery{evq("signup"), evq("product_viewed")},
				}),

			insightTile("Orders: members vs everyone else", "Purchases each week, split by Pug Club membership.",
				viewBarGroup, cell(0, 66, wFull, hStd),
				split(trend(evq("purchase")), "membership", 2)),

			insightTile("From Club page to paid plan", "Club page, trial, subscription, first invoice within 14 days, with the time each step takes.",
				viewBarGroup, cell(0, 88, wTwoThird, hList),
				funnel(14*24*time.Hour, true,
					evq("page_view", pf("page_type", opEquals, srcCustom, "Club")),
					evq("trial_started"), evq("subscription_started"), evq("invoice_paid"))),
			insightTile("New Club members", "Pug Club subscriptions started each week.",
				viewBarGroup, cell(wTwoThird, 88, wThird, hList),
				trend(evq("subscription_started"))),

			insightTile("Club revenue by plan", "Paid Pug Club invoices each week, by plan.",
				viewLine, cell(0, 112, wFull, hStd),
				split(trend(evqAgg("invoice_paid", aggSum, "amount")), "plan_name", 3),
				withFormat(formatCompact)),

			insightTile("Top customers", "Customers ranked by orders, each with their dog's profile.",
				viewTable, cell(0, 134, wWide, hUsers),
				&insightsv1.InsightQuerySpec{
					InsightType: itTopK.Enum(),
					TopK: &insightsv1.TopKQuery{
						Dimension:  dimUser.Enum(),
						Metric:     aggTotal.Enum(),
						Scope:      efilter("purchase"),
						Limit:      proto.Int32(10),
						OmitOthers: proto.Bool(true),
					},
				}),
			insightTile("Customer satisfaction", "Average survey score out of 10. Green from 8 up.",
				viewKPI, cell(wWide, 134, wNarrow, hKPI),
				trend(evqAgg("nps_submitted", aggAvg, "score")),
				withThresholds(threshold(dashboardsv1.ThresholdRule_OPERATOR_GTE, 8, dashboardsv1.ThresholdRule_TONE_GOOD))),
			insightTile("What senior dogs buy", "Revenue by category from dogs aged 8 and up, a profile filter.",
				viewTable, cell(wWide, 150, wNarrow, hUsers-hKPI),
				where(ranked("category", aggSum, "amount", efilter("purchase"), 6, false),
					pfv("age_years", opBetween, srcProfile, "8", "16"))),
		},
	}
}

func appDashboard() dashDef {
	return dashDef{
		displayName: "Mobile app & push",
		description: "How the iOS and Android apps are doing, and what push notifications earn.",
		timeRange:   rangeLast30,
		granularity: granDay,
		tiles: []coredashboards.UpsertTileInput{
			markdownTile(
				"### 📱 Mobile app & push\n\n"+
					"Ship releases with confidence and see what push notifications earn.\n\n"+
					"- **Version adoption** as each release rolls out\n"+
					"- **Crashes per day**, spiking after each release\n"+
					"- **Push to purchase**, step by step\n"+
					"- **Where the app is used**, on a map\n\n"+
					"The web, iOS and Android SDKs feed the same profiles that push campaigns reach.\n",
				cell(0, 0, wThird, hLarge),
			),
			insightTile("App version adoption", "Unique app users by version, as each release rolls out.",
				viewBarStack, cell(wThird, 0, wTwoThird, hLarge),
				split(trend(evqAgg("app_open", aggUniqueUser, "")), "$app_version", 4)),

			insightTile("Crashes per day", "App crashes on iOS and Android.",
				viewBarGroup, cell(0, 26, wWide, hUsers),
				trend(evq("app_crashed"))),
			insightTile("App cold start", "Average time from tap to first screen, against the previous 30 days.",
				viewKPI, cell(wWide, 26, wNarrow, hKPI),
				trend(evqAgg("app_open", aggAvg, "cold_start_ms")),
				withCompare(), withFormat(formatDuration)),
			insightTile("iOS vs Android", "Unique app users by operating system.",
				viewPie, cell(wWide, 42, wNarrow, hUsers-hKPI),
				split(trend(evqAgg("app_open", aggUniqueUser, "")), "$os", 2)),

			insightTile("Push to purchase", "Received, opened, app opened, purchased within an hour.",
				viewBarGroup, cell(0, 62, wWide, hList),
				funnel(time.Hour, true, evq("notification_received"), evq("notification_clicked"), evq("app_open"), evq("purchase"))),
			insightTile("Push opens", "Notifications opened each day.",
				viewArea, cell(wWide, 62, wNarrow, hList),
				trend(evq("notification_clicked"))),

			insightTile("Where the app is used", "Unique app users by country.",
				viewMap, cell(0, 86, wWide, hLarge),
				countryMap(efilter("app_open"), aggUniqueUser, "")),
			insightTile("Top errors", "Error codes on every platform, ranked.",
				viewTable, cell(wWide, 86, wNarrow, hLarge),
				ranked("error_code", aggTotal, "", efilter("error_occurred"), 10, false)),
		},
	}
}

// SeedDemoDashboards creates each board missing by display name and upserts the
// rest in place. It never deletes a board, so one dropped from demoDashboards
// stays until the demo project is rebuilt.
func SeedDemoDashboards(ctx context.Context, pg *pgxpool.Pool, projectID string) error {
	svc := coredashboards.NewService(pg, pg)

	existing, err := svc.ListDashboards(ctx, projectID)
	if err != nil {
		return err
	}
	idByName := make(map[string]string, len(existing))
	for _, d := range existing {
		idByName[d.Dashboard.DisplayName] = d.Dashboard.ID
	}

	for _, def := range demoDashboards() {
		dashboardID, ok := idByName[def.displayName]
		if !ok {
			created, err := svc.CreateDashboard(ctx, projectID, def.displayName, def.description, def.timeRange, def.granularity)
			if err != nil {
				return err
			}
			dashboardID = created.ID
		}
		if _, err := svc.UpsertDashboard(ctx, projectID, dashboardID, coredashboards.UpsertDashboardInput{
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
