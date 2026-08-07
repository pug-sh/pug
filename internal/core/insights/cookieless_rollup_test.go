package insights

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
)

// The rollup predicate renders as `cookieless = ?` (arg 0); its absence means
// the query merges rows across both key values.
const rollupCookielessPred = "cookieless = ?"

func TestCookielessExclusion_RollupBuilders(t *testing.T) {
	uu := insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS

	t.Run("trends_unique_users_filters_flag", func(t *testing.T) {
		req := rollupDayReq(rollupTrendsSpec(uu, "page_view", ""))
		q, err := buildTrendsFromRollup(req, "proj_123")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(q.SQL(), rollupCookielessPred) {
			t.Errorf("rollup UNIQUE_USERS must filter the flag column:\n%s", q.SQL())
		}
	})

	t.Run("trends_total_reads_all_rows", func(t *testing.T) {
		req := rollupDayReq(rollupTrendsSpec(insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL, "page_view", ""))
		q, err := buildTrendsFromRollup(req, "proj_123")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(q.SQL(), "cookieless") {
			t.Errorf("rollup TOTAL must merge both flag values:\n%s", q.SQL())
		}
	})

	t.Run("trends_breakdown_top_vals_ranks_same_population", func(t *testing.T) {
		req := rollupDayReq(rollupTrendsSpec(uu, "page_view", "$country"))
		q, err := buildTrendsFromRollup(req, "proj_123")
		if err != nil {
			t.Fatal(err)
		}
		// Predicate must appear in BOTH the top_vals CTE and the per-event query.
		if got := strings.Count(q.SQL(), rollupCookielessPred); got != 2 {
			t.Errorf("expected flag predicate in top_vals CTE and event query (2), got %d:\n%s", got, q.SQL())
		}
	})

	t.Run("mixed_agg_breakdown_ranking_is_order_independent", func(t *testing.T) {
		// The ranking CTE once keyed its cookieless predicate off aggregationType(req)
		// — events[0] ONLY — while each branch keyed off its own aggregation. With one
		// TOTAL and one UNIQUE_USERS event, swapping the `events` order flipped whether
		// the ranking population excluded cookieless traffic, so two dashboard tiles
		// differing only in event order named different breakdown values. Nothing in
		// InsightQuerySpec's CEL rules requires uniform aggregation across events, and
		// the raw path supports mixed explicitly (builder.go: sibling TOTAL branches
		// must keep counting all traffic), so this shape is reachable and valid.
		pv := rollupEventSpec{"page_view", insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL}
		su := rollupEventSpec{"signup", uu}

		forward, err := buildTrendsFromRollup(rollupDayReq(rollupTrendsSpecFor("$country", pv, su)), "proj_123")
		if err != nil {
			t.Fatal(err)
		}
		reversed, err := buildTrendsFromRollup(rollupDayReq(rollupTrendsSpecFor("$country", su, pv)), "proj_123")
		if err != nil {
			t.Fatal(err)
		}

		// The predicate must follow the AGGREGATION, never the position: the
		// UNIQUE_USERS event's ranking CTE restricts, the TOTAL event's does not,
		// in both orderings. Counting occurrences alone cannot catch this.
		//
		// Asserted on top_grain_<i>, which is where the scan (and therefore the
		// population predicate) lives; top_vals_<i> only sums that CTE's output.
		for _, c := range []struct {
			order      string
			sql        string
			restricted string // grain CTE of the UNIQUE_USERS event
			open       string // grain CTE of the TOTAL event
		}{
			{"forward", forward.SQL(), "top_grain_1", "top_grain_0"},
			{"reversed", reversed.SQL(), "top_grain_0", "top_grain_1"},
		} {
			if !strings.Contains(cteBody(t, c.sql, c.restricted), rollupCookielessPred) {
				t.Errorf("%s: UNIQUE_USERS event's ranking CTE (%s) must exclude cookieless:\n%s", c.order, c.restricted, c.sql)
			}
			if strings.Contains(cteBody(t, c.sql, c.open), rollupCookielessPred) {
				t.Errorf("%s: TOTAL event's ranking CTE (%s) must rank over all traffic:\n%s", c.order, c.open, c.sql)
			}
		}
	})

	t.Run("toggle_keeps_fast_path", func(t *testing.T) {
		spec := rollupTrendsSpec(uu, "page_view", "")
		spec.IncludeCookieless = proto.Bool(true)
		req := rollupDayReq(spec)
		q, usedRollup, err := trendsQueryForExecution(req, "proj_123", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if !usedRollup {
			t.Error("include_cookieless=true must NOT force the raw path")
		}
		if strings.Contains(q.SQL(), "cookieless") {
			t.Errorf("included: no flag predicate, states merge:\n%s", q.SQL())
		}
	})

	t.Run("exclusion_keeps_fast_path", func(t *testing.T) {
		req := rollupDayReq(rollupTrendsSpec(uu, "page_view", ""))
		_, usedRollup, err := trendsQueryForExecution(req, "proj_123", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if !usedRollup {
			t.Error("default exclusion must stay on the rollup fast path (flag is a key column)")
		}
	})

	t.Run("segmentation_unique_users_filters_flag", func(t *testing.T) {
		req := rollupDayReq(rollupSegSpec(uu, "page_view"))
		q, err := buildSegmentationFromRollup(req, "proj_123")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(q.SQL(), rollupCookielessPred) {
			t.Errorf("rollup segmentation UNIQUE_USERS must filter the flag column:\n%s", q.SQL())
		}
	})

	t.Run("topk_unique_users_filters_flag_total_does_not", func(t *testing.T) {
		uuReq := rollupDayReq(rollupTopKSpec(insightsv1.TopKQuery_DIMENSION_PROPERTY, "$pathname", "", uu))
		q, err := buildTopKFromRollup(uuReq, "proj_123")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(q.SQL(), rollupCookielessPred) {
			t.Errorf("rollup topK UNIQUE_USERS must filter the flag column:\n%s", q.SQL())
		}

		totReq := rollupDayReq(rollupTopKSpec(insightsv1.TopKQuery_DIMENSION_PROPERTY, "$pathname", "", insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL))
		q, err = buildTopKFromRollup(totReq, "proj_123")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(q.SQL(), "cookieless") {
			t.Errorf("rollup topK TOTAL must merge both flag values:\n%s", q.SQL())
		}
	})
}
