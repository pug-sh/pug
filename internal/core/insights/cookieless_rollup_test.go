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
