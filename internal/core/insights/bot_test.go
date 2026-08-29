package insights

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	chq "github.com/pug-sh/pug/internal/core/clickhouse"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
)

// "e.bot = 0" contains "bot = 0": the unaliased needle matches either row-level
// form, the aliased one pins the identity-join form.
const (
	botNeedle        = "bot = 0"
	botNeedleAliased = "e.bot = 0"
	botSessionNeedle = "max(bot) = 0"
)

func hasBotPredicate(sql string) bool {
	return strings.Contains(sql, botNeedle) || strings.Contains(sql, botSessionNeedle)
}

func TestExcludeBots(t *testing.T) {
	if !excludeBots(&insightsv1.InsightQuerySpec{}) {
		t.Error("bots must be excluded by default")
	}
	if excludeBots(&insightsv1.InsightQuerySpec{IncludeBots: proto.Bool(true)}) {
		t.Error("include_bots=true must lift the exclusion")
	}
}

func TestBotExclusionCond(t *testing.T) {
	if c := botExclusionCond(false, ""); !c.IsZero() {
		t.Errorf("no-exclusion must be the zero condition (skipped by Where), got %q", c.SQL())
	}
	if c := botExclusionCond(true, ""); c.SQL() != botNeedle || len(c.Args()) != 0 {
		t.Errorf("unaliased cond = %q args=%v, want %q with no args", c.SQL(), c.Args(), botNeedle)
	}
	if c := botExclusionCond(true, "e"); c.SQL() != botNeedleAliased {
		t.Errorf("aliased cond = %q, want %q", c.SQL(), botNeedleAliased)
	}
}

func TestBotSessionHaving(t *testing.T) {
	q := chq.NewQuery().Select("session_id").From("events").GroupBy("session_id")
	botSessionHaving(q, true)
	sql, _, err := q.Build()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, "HAVING "+botSessionNeedle) {
		t.Errorf("exclude must add the session-level HAVING:\n%s", sql)
	}
	q = chq.NewQuery().Select("session_id").From("events").GroupBy("session_id")
	botSessionHaving(q, false)
	sql, _, err = q.Build()
	if err != nil {
		t.Fatal(err)
	}
	if hasBotPredicate(sql) {
		t.Errorf("include must add nothing:\n%s", sql)
	}
}

// botScan pins the predicate per source scan, not by presence: a funnel with
// it on one of two scans still returns bot people.
type botScan struct {
	name   string
	sql    string
	needle string
	scans  int
}

func botScanTrendsSpec(agg insightsv1.AggregationType, include bool) *insightsv1.InsightQuerySpec {
	spec := rollupTrendsSpec(agg, "page_view", "$country")
	spec.IncludeBots = proto.Bool(include)
	return spec
}

// Ranges over every InsightType: a type added without a row here fails, as
// does a builder that drops the predicate from one of its scans. UNSPECIFIED
// is the only non-buildable member.
func TestBotExclusion_EveryInsightType(t *testing.T) {
	uu := insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS
	must := func(t *testing.T, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	cases := map[insightsv1.InsightType]func(t *testing.T, include bool) []botScan{
		insightsv1.InsightType_INSIGHT_TYPE_UNSPECIFIED: nil,

		insightsv1.InsightType_INSIGHT_TYPE_TRENDS: func(t *testing.T, include bool) []botScan {
			req := rollupDayReq(botScanTrendsSpec(uu, include))
			raw, err := BuildTrendsQuery(req, "p1")
			must(t, err)
			rollup, err := buildTrendsFromRollup(req, "p1")
			must(t, err)
			return []botScan{
				{"raw", raw.SQL(), botNeedle, 1},
				// The breakdown ranking CTE and the per-event query both scan the
				// rollup; ranking over bot traffic would name the wrong top N.
				{"rollup", rollup.SQL(), botNeedle, 2},
			}
		},

		insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION: func(t *testing.T, include bool) []botScan {
			spec := rollupSegSpec(uu, "page_view")
			spec.IncludeBots = proto.Bool(include)
			req := rollupDayReq(spec)
			raw, err := BuildSegmentationQuery(req, "p1")
			must(t, err)
			rollup, err := buildSegmentationFromRollup(req, "p1")
			must(t, err)
			return []botScan{
				{"raw", raw.SQL(), botNeedle, 1},
				{"rollup", rollup.SQL(), botNeedle, 1},
			}
		},

		insightsv1.InsightType_INSIGHT_TYPE_FUNNEL: func(t *testing.T, include bool) []botScan {
			req := rollupDayReq(&insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
				Events: []*insightsv1.EventQuery{
					{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}},
					{Event: &commonv1.EventFilter{Kind: proto.String("purchase")}},
				},
				IncludeStepTiming: proto.Bool(true),
				IncludeBots:       proto.Bool(include),
			})
			counts, err := BuildFunnelCountsQuery(req, "p1")
			must(t, err)
			timing, err := BuildFunnelTimingQuery(req, "p1")
			must(t, err)
			return []botScan{
				{"counts", counts.SQL(), botNeedleAliased, 1},
				// windowFunnel pre-filter + tagged CTE.
				{"timing", timing.SQL(), botNeedleAliased, 2},
			}
		},

		insightsv1.InsightType_INSIGHT_TYPE_RETENTION: func(t *testing.T, include bool) []botScan {
			req := rollupDayReq(&insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_RETENTION.Enum(),
				Events:      []*insightsv1.EventQuery{{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}}},
				IncludeBots: proto.Bool(include),
			})
			q, err := BuildRetentionQuery(req, "p1")
			must(t, err)
			// cohorts + return_events.
			return []botScan{{"raw", q.SQL(), botNeedleAliased, 2}}
		},

		insightsv1.InsightType_INSIGHT_TYPE_USER_FLOW: func(t *testing.T, include bool) []botScan {
			req := rollupDayReq(&insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_USER_FLOW.Enum(),
				UserFlow:    &insightsv1.UserFlowQuery{},
				IncludeBots: proto.Bool(include),
			})
			q, err := BuildUserFlowQuery(req, "p1")
			must(t, err)
			// Groups by session, so the predicate is session-level — a row-level
			// one would leave a straddling session as a truncated path.
			return []botScan{{"raw", q.SQL(), botSessionNeedle, 1}}
		},

		insightsv1.InsightType_INSIGHT_TYPE_TOP_K: func(t *testing.T, include bool) []botScan {
			spec := rollupTopKSpec(insightsv1.TopKQuery_DIMENSION_PROPERTY, "$pathname", "", uu)
			spec.IncludeBots = proto.Bool(include)
			req := rollupDayReq(spec)
			raw, err := BuildTopKQuery(req, "p1")
			must(t, err)
			rollup, err := buildTopKFromRollup(req, "p1")
			must(t, err)
			userSpec := rollupTopKSpec(insightsv1.TopKQuery_DIMENSION_USER, "", "", insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL)
			userSpec.IncludeBots = proto.Bool(include)
			users, err := BuildTopKQuery(rollupDayReq(userSpec), "p1")
			must(t, err)
			return []botScan{
				// top_vals CTE + outer re-aggregation share one condition set.
				{"raw", raw.SQL(), botNeedle, 2},
				{"rollup", rollup.SQL(), botNeedle, 2},
				{"users", users.SQL(), botNeedleAliased, 1},
			}
		},

		insightsv1.InsightType_INSIGHT_TYPE_MAP: func(t *testing.T, include bool) []botScan {
			req := mapRequest(&insightsv1.MapQuery{})
			req.Spec.IncludeBots = proto.Bool(include)
			topK := topKRequestForMap(req)
			raw, err := BuildTopKQuery(topK, "p1")
			must(t, err)
			rollup, err := buildTopKFromRollup(topK, "p1")
			must(t, err)
			// A map omits $others, so both shapes are a single scan.
			return []botScan{
				{"raw", raw.SQL(), botNeedle, 1},
				{"rollup", rollup.SQL(), botNeedle, 1},
			}
		},
	}

	for num, name := range insightsv1.InsightType_name {
		it := insightsv1.InsightType(num)
		build, ok := cases[it]
		if !ok {
			t.Errorf("%s has no bot decision: every insight must exclude bots by default — add it to this table and thread botExclusionCond (or botSessionHaving) through its builders", name)
			continue
		}
		if build == nil {
			continue
		}
		t.Run(name, func(t *testing.T) {
			for _, s := range build(t, false) {
				if got := strings.Count(s.sql, s.needle); got != s.scans {
					t.Errorf("%s: default must carry %q on every scan: want %d, got %d:\n%s", s.name, s.needle, s.scans, got, s.sql)
				}
			}
			for _, s := range build(t, true) {
				if hasBotPredicate(s.sql) {
					t.Errorf("%s: include_bots=true must emit no bot predicate:\n%s", s.name, s.sql)
				}
			}
		})
	}
}

// Every session metric excludes at the session level on both paths; a
// row-level `bot = 0` would keep the untagged half of a straddling session,
// so its absence is asserted too.
func TestBotExclusion_EverySessionMetric(t *testing.T) {
	buildable := map[insightsv1.SessionMetric]bool{
		insightsv1.SessionMetric_SESSION_METRIC_UNSPECIFIED:            false,
		insightsv1.SessionMetric_SESSION_METRIC_SESSIONS:               true,
		insightsv1.SessionMetric_SESSION_METRIC_AVG_DURATION:           true,
		insightsv1.SessionMetric_SESSION_METRIC_BOUNCE_RATE:            true,
		insightsv1.SessionMetric_SESSION_METRIC_ENTRY:                  true,
		insightsv1.SessionMetric_SESSION_METRIC_EXIT:                   true,
		insightsv1.SessionMetric_SESSION_METRIC_AVG_EVENTS_PER_SESSION: true,
	}

	for num, name := range insightsv1.SessionMetric_name {
		metric := insightsv1.SessionMetric(num)
		ok, decided := buildable[metric]
		if !decided {
			t.Errorf("%s has no session-bot decision: add it to buildable — a session metric must exclude bot sessions whole", name)
			continue
		}
		if !ok {
			continue
		}
		t.Run(name, func(t *testing.T) {
			for _, include := range []bool{false, true} {
				req := sessionReq(metric, "page_view", "$pathname")
				req.Spec.IncludeBots = proto.Bool(include)

				rawTrends, err := BuildSessionTrendsQuery(req, "p1")
				if err != nil {
					t.Fatal(err)
				}
				rawSeg, err := BuildSessionSegmentationQuery(req, "p1")
				if err != nil {
					t.Fatal(err)
				}
				rollupTrends, err := buildSessionTrendsFromRollup(req, "p1")
				if err != nil {
					t.Fatal(err)
				}
				rollupSeg, err := buildSessionSegmentationFromRollup(req, "p1")
				if err != nil {
					t.Fatal(err)
				}
				for _, s := range []struct{ name, sql string }{
					{"raw trends", rawTrends.SQL()},
					{"raw segmentation", rawSeg.SQL()},
					{"rollup trends", rollupTrends.SQL()},
					{"rollup segmentation", rollupSeg.SQL()},
				} {
					switch {
					case include && hasBotPredicate(s.sql):
						t.Errorf("%s: include_bots=true must emit no bot predicate:\n%s", s.name, s.sql)
					case !include && strings.Count(s.sql, botSessionNeedle) != 1:
						t.Errorf("%s: default must exclude at the session level (%q once):\n%s", s.name, botSessionNeedle, s.sql)
					case !include && strings.Contains(s.sql, botNeedle):
						t.Errorf("%s: a row-level predicate keeps half of a straddling session:\n%s", s.name, s.sql)
					}
				}
			}
		})
	}
}

// Both toggle states must stay on the fast path: bot is a key column on both
// rollups, so exclusion is a predicate and inclusion is a merge across the
// key's two values — never a reason to fall back to raw events.
func TestBotExclusion_KeepsFastPath(t *testing.T) {
	uu := insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS
	now := time.Now()
	for _, include := range []bool{false, true} {
		trendsSpec := rollupTrendsSpec(uu, "page_view", "")
		trendsSpec.IncludeBots = proto.Bool(include)
		if _, usedRollup, err := trendsQueryForExecution(rollupDayReq(trendsSpec), "p1", now); err != nil || !usedRollup {
			t.Errorf("include=%v trends: usedRollup=%v err=%v", include, usedRollup, err)
		}

		segSpec := rollupSegSpec(uu, "page_view")
		segSpec.IncludeBots = proto.Bool(include)
		if _, usedRollup, err := segmentationQueryForExecution(rollupDayReq(segSpec), "p1", now); err != nil || !usedRollup {
			t.Errorf("include=%v segmentation: usedRollup=%v err=%v", include, usedRollup, err)
		}

		topKSpec := rollupTopKSpec(insightsv1.TopKQuery_DIMENSION_PROPERTY, "$pathname", "", uu)
		topKSpec.IncludeBots = proto.Bool(include)
		if _, usedRollup, err := topKQueryForExecution(rollupDayReq(topKSpec), "p1", now); err != nil || !usedRollup {
			t.Errorf("include=%v top k: usedRollup=%v err=%v", include, usedRollup, err)
		}

		sessionReq := sessionReq(insightsv1.SessionMetric_SESSION_METRIC_SESSIONS, "page_view", "")
		sessionReq.Spec.IncludeBots = proto.Bool(include)
		if _, usedRollup, err := sessionTrendsQueryForExecution(sessionReq, "p1", now); err != nil || !usedRollup {
			t.Errorf("include=%v session trends: usedRollup=%v err=%v", include, usedRollup, err)
		}
	}
}

// Multi-event trends put the predicate in the shared WHERE once — unlike
// cookieless, which is per branch because a TOTAL sibling keeps that traffic.
func TestBotExclusion_MultiEventTrendsSharedWhere(t *testing.T) {
	spec := rollupMultiEventTrendsSpec("page_view", "signup")
	spec.Events[1].Aggregation = insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS.Enum()
	q, err := BuildTrendsQuery(rollupDayReq(spec), "p1")
	if err != nil {
		t.Fatal(err)
	}
	sql := q.SQL()
	if !strings.Contains(sql, "countIf") {
		t.Fatalf("expected the single-scan multi-event shape:\n%s", sql)
	}
	if got := strings.Count(sql, botNeedle); got != 1 {
		t.Errorf("predicate must appear exactly once, in the shared WHERE, got %d:\n%s", got, sql)
	}
}

// SegmentUsers enumerates "who is behind this number" and carries no
// InsightQuerySpec, so the exclusion is unconditional, as for cookieless ids.
func TestBuildSegmentUsersQuery_ExcludesBots(t *testing.T) {
	sql, _, err := BuildSegmentUsersQuery(&insightsv1.SegmentUsersRequest{
		Events:    []*insightsv1.EventQuery{{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}}},
		TimeRange: rollupTimeRange("2024-01-01T00:00:00Z", "2024-01-08T00:00:00Z"),
	}, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, botNeedle) {
		t.Errorf("SegmentUsers must exclude bot-tagged events:\n%s", sql)
	}
}
