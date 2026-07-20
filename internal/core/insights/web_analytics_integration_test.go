package insights_test

import (
	"context"
	"os"
	"reflect"
	"regexp"
	"strings"
	"strconv"
	"testing"
	"time"

	chcol "github.com/ClickHouse/clickhouse-go/v2/lib/chcol"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/pug-sh/pug/internal/attribution"
	chq "github.com/pug-sh/pug/internal/core/clickhouse"
	"github.com/pug-sh/pug/internal/core/insights"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/testutil"
)

// TestIntegrationWebAnalytics covers the migration-008/009/010 web-analytics
// surfaces: the 008 derivation mutation's parity with attribution.Derive, the
// 010 partial-column backfill's merge-identity safety, and rollup-vs-raw
// parity on the new event and session dimensions + AVG_EVENTS_PER_SESSION.
func TestIntegrationWebAnalytics(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()
	executor := insights.NewExecutor(ch.Conn)

	// The mutation subtest re-runs 008's ALTER UPDATE over the whole events
	// table, so it runs FIRST: later subtests' rows are inserted by the
	// post-008 code path (columns already populated) and must not be visible
	// to the corpus assertions (dedicated project ids keep them apart).
	t.Run("mutation_008_matches_attribution_derive", func(t *testing.T) {
		testMutation008DeriveParity(t, ctx, ch)
	})

	t.Run("session_rollup_partial_insert_merge_identity", func(t *testing.T) {
		testSessionRollupPartialInsertMergeIdentity(t, ctx, ch)
	})

	// Also table-wide (its DELETE + INSERT rebuild every project's new dims
	// from `events`), so it runs before the parity seeds below.
	t.Run("event_rollup_backfill_009_rerunnable", func(t *testing.T) {
		testBackfill009Rerunnable(t, ctx, ch)
	})

	const webRollProject = "proj_webroll"
	seedWebEventGrain(t, ctx, ch, webRollProject)

	t.Run("rollup_parity_trends_pathname_breakdown", func(t *testing.T) {
		req := webTrendsReq(insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL, "page_view", "$pathname")
		assertTrendsParityBy(t, ctx, executor, webRollProject, req, "$pathname")
	})

	t.Run("rollup_parity_trends_channel_unique_users", func(t *testing.T) {
		req := webTrendsReq(insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS, "page_view", "$channel")
		assertTrendsParityBy(t, ctx, executor, webRollProject, req, "$channel")
	})

	// $screenSize and $utmTerm are event-rollup dims like any other; they get
	// their own breakdown because the seeds above are the only place they are
	// populated, so nothing else would carry them through the MV.
	t.Run("rollup_parity_trends_screen_size", func(t *testing.T) {
		req := webTrendsReq(insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL, "page_view", "$screenSize")
		got := assertTrendsParityBy(t, ctx, executor, webRollProject, req, "$screenSize")
		// alice+carol desktop on Jan 1, bob mobile.
		for key, want := range map[string]float64{
			"page_view|1920x1080|2024-01-01": 2, "page_view|390x844|2024-01-01": 1,
		} {
			if got[key] != want {
				t.Errorf("screen size %s = %v, want %v (all: %v)", key, got[key], want, got)
			}
		}
	})

	t.Run("rollup_parity_trends_utm_term_unique_users", func(t *testing.T) {
		req := webTrendsReq(insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS, "page_view", "$utmTerm")
		assertTrendsParityBy(t, ctx, executor, webRollProject, req, "$utmTerm")
	})

	t.Run("rollup_parity_topk_pathname_and_channel", func(t *testing.T) {
		cases := []struct {
			name string
			tk   *insightsv1.TopKQuery
		}{
			{"pathname_total_scoped", &insightsv1.TopKQuery{
				Dimension: insightsv1.TopKQuery_DIMENSION_PROPERTY.Enum(),
				Property:  proto.String("$pathname"),
				Scope:     &commonv1.EventFilter{Kind: proto.String("page_view")},
				Limit:     proto.Int32(2),
			}},
			{"channel_unique_users", &insightsv1.TopKQuery{
				Dimension: insightsv1.TopKQuery_DIMENSION_PROPERTY.Enum(),
				Property:  proto.String("$channel"),
				Metric:    insightsv1.AggregationType_AGGREGATION_TYPE_UNIQUE_USERS.Enum(),
				Limit:     proto.Int32(2),
			}},
			{"screen_size_total", &insightsv1.TopKQuery{
				Dimension: insightsv1.TopKQuery_DIMENSION_PROPERTY.Enum(),
				Property:  proto.String("$screenSize"),
				Scope:     &commonv1.EventFilter{Kind: proto.String("page_view")},
				Limit:     proto.Int32(2),
			}},
			{"utm_term_total", &insightsv1.TopKQuery{
				Dimension: insightsv1.TopKQuery_DIMENSION_PROPERTY.Enum(),
				Property:  proto.String("$utmTerm"),
				Scope:     &commonv1.EventFilter{Kind: proto.String("page_view")},
				Limit:     proto.Int32(2),
			}},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				req := &insightsv1.QueryRequest{
					Spec: &insightsv1.InsightQuerySpec{
						InsightType: insightsv1.InsightType_INSIGHT_TYPE_TOP_K.Enum(),
						TopK:        c.tk,
					},
					TimeRange:   webWindow(),
					Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
				}
				rawQ, err := insights.BuildTopKQuery(req, webRollProject)
				if err != nil {
					t.Fatalf("BuildTopKQuery (raw): %v", err)
				}
				rawRows, err := executor.QueryTopK(ctx, webRollProject, rawQ)
				if err != nil {
					t.Fatalf("QueryTopK (raw): %v", err)
				}
				resp, err := insights.ExecuteQuery(ctx, executor, webRollProject, req, time.Now())
				if err != nil {
					t.Fatalf("ExecuteQuery (rollup): %v", err)
				}
				var rollupRows []insights.TopKRow
				for _, r := range resp.GetTopK().GetRows() {
					rollupRows = append(rollupRows, insights.TopKRow{
						DimensionValue: r.GetDimensionValue(),
						IsOthers:       r.GetIsOthers(),
						Value:          r.GetValue(),
					})
				}
				if !reflect.DeepEqual(rawRows, rollupRows) {
					t.Errorf("raw and rollup top-K diverge:\nraw:    %+v\nrollup: %+v", rawRows, rollupRows)
				}
				if len(rollupRows) == 0 {
					t.Error("empty top-K result — seed/window mismatch would make this parity check vacuous")
				}
			})
		}
	})

	const webSessProject = "proj_websess"
	seedWebSessionGrain(t, ctx, ch, webSessProject)

	// Both metrics need their OWN value assertion, not just rollup==raw parity:
	// the rollup and raw builders each special-case ENTRY vs EXIT, so a
	// symmetric argMin/argMax inversion flips both sides identically and parity
	// alone stays green while every Entry and Exit page swaps places.
	t.Run("session_rollup_parity_entry_exit_pathname", func(t *testing.T) {
		for _, tc := range []struct {
			metric insightsv1.SessionMetric
			want   map[string]float64
		}{
			// S1 entered on /landing, S2 on /home (both Jan 1); S3 on /a (Jan 2).
			{insightsv1.SessionMetric_SESSION_METRIC_ENTRY, map[string]float64{
				"/landing|2024-01-01": 1, "/home|2024-01-01": 1, "/a|2024-01-02": 1,
			}},
			// S1 left from /checkout, S2 bounced on /home (both Jan 1); S3 left
			// from /c (Jan 2).
			{insightsv1.SessionMetric_SESSION_METRIC_EXIT, map[string]float64{
				"/checkout|2024-01-01": 1, "/home|2024-01-01": 1, "/c|2024-01-02": 1,
			}},
		} {
			req := webSessionTrendsReq(tc.metric, "$pathname", "")
			got := assertSessionTrendsParityBy(t, ctx, executor, webSessProject, req, "$pathname")
			for key, want := range tc.want {
				if got[key] != want {
					t.Errorf("%v pathname %s = %v, want %v (all: %v)", tc.metric, key, got[key], want, got)
				}
			}
		}
	})

	t.Run("session_rollup_parity_sessions_by_channel", func(t *testing.T) {
		req := webSessionTrendsReq(insightsv1.SessionMetric_SESSION_METRIC_SESSIONS, "$channel", "")
		got := assertSessionTrendsParityBy(t, ctx, executor, webSessProject, req, "$channel")
		// First-touch: S1 landed Organic Search, S2 Direct (Jan 1); S3 Organic Social (Jan 2).
		for key, want := range map[string]float64{
			"Organic Search|2024-01-01": 1, "Direct|2024-01-01": 1, "Organic Social|2024-01-02": 1,
		} {
			if got[key] != want {
				t.Errorf("sessions by channel %s = %v, want %v (all: %v)", key, got[key], want, got)
			}
		}
	})

	t.Run("session_rollup_parity_sessions_by_referrer_domain", func(t *testing.T) {
		req := webSessionTrendsReq(insightsv1.SessionMetric_SESSION_METRIC_SESSIONS, "$referrerDomain", "")
		assertSessionTrendsParityBy(t, ctx, executor, webSessProject, req, "$referrerDomain")
	})

	t.Run("session_rollup_parity_avg_events_per_session", func(t *testing.T) {
		// Trends parity (no breakdown).
		req := webSessionTrendsReq(insightsv1.SessionMetric_SESSION_METRIC_AVG_EVENTS_PER_SESSION, "", "page_view")
		assertSessionTrendsParityBy(t, ctx, executor, webSessProject, req, "")

		// Segmentation parity + sanity: sessions carry 2, 1, and 3 page_views
		// → pages/session = 2 over the window.
		seg := &insightsv1.QueryRequest{
			Spec: &insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION.Enum(),
				Session: &insightsv1.SessionQuery{
					Metric: insightsv1.SessionMetric_SESSION_METRIC_AVG_EVENTS_PER_SESSION.Enum(),
					Scope:  &commonv1.EventFilter{Kind: proto.String("page_view")},
				},
			},
			TimeRange:   webWindow(),
			Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
		}
		resp, err := insights.ExecuteQuery(ctx, executor, webSessProject, seg, time.Now())
		if err != nil {
			t.Fatalf("ExecuteQuery (rollup): %v", err)
		}
		rollup := resp.GetSegmentation().GetTotal()

		rawQ, err := insights.BuildSessionSegmentationQuery(seg, webSessProject)
		if err != nil {
			t.Fatalf("BuildSessionSegmentationQuery (raw): %v", err)
		}
		raw, err := executor.QueryScalar(ctx, webSessProject, rawQ)
		if err != nil {
			t.Fatalf("QueryScalar (raw): %v", err)
		}
		if rollup != raw {
			t.Errorf("avg events/session rollup=%v raw=%v", rollup, raw)
		}
		if rollup != 2 {
			t.Errorf("avg events/session = %v, want 2", rollup)
		}
	})

	const webMixedProject = "proj_webmixed"
	seedWebMixedKindGrain(t, ctx, ch, webMixedProject)

	// The session rollup keys on kind, and the session-grain panels scope
	// kind=page_view precisely so a URL-less first event cannot claim a
	// session's first-touch attribution (web-analytics.md). A project seeded
	// with page_view alone leaves the empty-kind and kind='page_view' rows carrying
	// identical state, so every kind-scoped assertion above passes without ever
	// discriminating the scope. These two assert the scopes DISAGREE.
	t.Run("session_rollup_kind_scope_is_discriminating", func(t *testing.T) {
		t.Run("entry_pathname", func(t *testing.T) {
			scoped := assertSessionTrendsParityBy(t, ctx, executor,
				webMixedProject, webMixedSessionTrendsReq(insightsv1.SessionMetric_SESSION_METRIC_ENTRY, "$pathname", "page_view"), "$pathname")
			unscoped := assertSessionTrendsParityBy(t, ctx, executor,
				webMixedProject, webMixedSessionTrendsReq(insightsv1.SessionMetric_SESSION_METRIC_ENTRY, "$pathname", ""), "$pathname")

			// Scoped: the session's first PAGEVIEW is /landing.
			if scoped["/landing|2024-04-01"] != 1 {
				t.Errorf("kind=page_view entry = %v, want /landing|2024-04-01 = 1", scoped)
			}
			// Unscoped: the URL-less identify event is first, so it claims entry
			// with an empty pathname — exactly the first-touch theft the scope
			// exists to prevent.
			if unscoped["|2024-04-01"] != 1 {
				t.Errorf("unscoped entry = %v, want the URL-less event to claim it with an empty pathname", unscoped)
			}
			if reflect.DeepEqual(scoped, unscoped) {
				t.Errorf("kind=page_view and unscoped entry states are identical (%v) — the rollup's kind routing is untested", scoped)
			}
		})

		t.Run("avg_events_per_session", func(t *testing.T) {
			scoped := sessionSegmentationValue(t, ctx, executor, webMixedProject, "page_view")
			unscoped := sessionSegmentationValue(t, ctx, executor, webMixedProject, "")
			// 2 page_views vs 2 page_views + 1 identify, over one session.
			if scoped != 2 {
				t.Errorf("kind=page_view pages/session = %v, want 2", scoped)
			}
			if unscoped != 3 {
				t.Errorf("unscoped events/session = %v, want 3", unscoped)
			}
		})
	})
}

// sessionSegmentationValue runs AVG_EVENTS_PER_SESSION segmentation through the
// rollup and asserts it against the raw builder, returning the agreed value.
func sessionSegmentationValue(t *testing.T, ctx context.Context, executor *insights.Executor, projectID, scopeKind string) float64 {
	t.Helper()
	session := &insightsv1.SessionQuery{Metric: insightsv1.SessionMetric_SESSION_METRIC_AVG_EVENTS_PER_SESSION.Enum()}
	if scopeKind != "" {
		session.Scope = &commonv1.EventFilter{Kind: proto.String(scopeKind)}
	}
	req := &insightsv1.QueryRequest{
		Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION.Enum(),
			Session:     session,
		},
		TimeRange:   webMixedWindow(),
		Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum(),
	}
	resp, err := insights.ExecuteQuery(ctx, executor, projectID, req, time.Now())
	if err != nil {
		t.Fatalf("ExecuteQuery (rollup): %v", err)
	}
	rollup := resp.GetSegmentation().GetTotal()

	rawQ, err := insights.BuildSessionSegmentationQuery(req, projectID)
	if err != nil {
		t.Fatalf("BuildSessionSegmentationQuery (raw): %v", err)
	}
	raw, err := executor.QueryScalar(ctx, projectID, rawQ)
	if err != nil {
		t.Fatalf("QueryScalar (raw): %v", err)
	}
	if rollup != raw {
		t.Errorf("kind=%q avg events/session rollup=%v raw=%v", scopeKind, rollup, raw)
	}
	return rollup
}

// ---------------------------------------------------------------------------
// 008 mutation ↔ attribution.Derive parity
// ---------------------------------------------------------------------------

// pre008InsertStmt inserts rows the way pre-008 binaries stored them: the
// web-analytics keys resident in the auto_properties map (never split), the
// long-promoted url/utm columns populated, and every new column at its
// empty DEFAULT.
const pre008InsertStmt = `INSERT INTO events (event_id, project_id, distinct_id, kind, auto_properties, custom_properties, url, utm_source, utm_medium, utm_campaign, occur_time, session_id)`

type mutationCorpusRow struct {
	name     string // doubles as distinct_id
	url      string
	referrer string
	// long-promoted columns (populated on pre-008 rows).
	utmSource, utmMedium, utmCampaign string
	// map-resident keys on pre-008 rows.
	locale, utmTerm, utmContent, pageTitle string
	screenW, screenH                       int64
	// Screen dims in the String variant slot. Client auto-properties are never
	// re-typed at ingest, so an SDK sending stringValue lands here — the
	// mutation must read this slot too or history derives nothing while live
	// traffic derives a size.
	screenWStr, screenHStr string
	// Screen dims in the Float64 variant slot: a JS SDK with no int/double
	// distinction sends window.screen.width as a double. autoprop.String
	// renders an integral double the same as the Int64 slot, so live traffic
	// derives a size — the mutation must read Float64 too or history diverges.
	screenWFloat, screenHFloat float64
}

// screenDim mirrors handler.go's autoPropInt64: it reads the first populated
// variant slot (Int64, then String, then Float64) as the integer the promotion
// layer would store, matching autoprop.String + ParseInt. Derive itself takes
// int64s, so this coercion is the handler's — and the 008 mutation mirrors the
// slots, not Derive alone.
func screenDim(n int64, s string, f float64) int64 {
	if n != 0 {
		return n
	}
	if s != "" {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0
		}
		return v
	}
	if f != 0 {
		// autoprop.String renders a double via FormatFloat('g'); ParseInt then
		// accepts it only when integral, rejecting a fractional dim.
		v, err := strconv.ParseInt(strconv.FormatFloat(f, 'g', -1, 64), 10, 64)
		if err != nil {
			return 0
		}
		return v
	}
	return 0
}

func (r mutationCorpusRow) deriveInput() attribution.Input {
	return attribution.Input{
		URL:          r.url,
		Referrer:     r.referrer,
		UTMSource:    r.utmSource,
		UTMMedium:    r.utmMedium,
		UTMCampaign:  r.utmCampaign,
		UTMTerm:      r.utmTerm,
		UTMContent:   r.utmContent,
		Locale:       r.locale,
		ScreenWidth:  screenDim(r.screenW, r.screenWStr, r.screenWFloat),
		ScreenHeight: screenDim(r.screenH, r.screenHStr, r.screenHFloat),
	}
}

var mutationCorpus = []mutationCorpusRow{
	{name: "plain_page", url: "https://pugandpals.example.com/products/ball", locale: "en-us", screenW: 1920, screenH: 1080, pageTitle: "Ball"},
	{name: "bare_host_root_path", url: "https://pugandpals.example.com", referrer: "https://www.google.com/"},
	{name: "uppercase_host_with_port", url: "https://Shop.Example.com:8443/Sale"},
	{name: "utm_from_url_query", url: "https://pugandpals.example.com/?utm_source=google&utm_medium=cpc&utm_term=dog+treats&utm_content=ad%201"},
	{name: "utm_columns_win_over_url", url: "https://pugandpals.example.com/?utm_source=bing", utmSource: "google", utmMedium: "cpc", utmCampaign: "summer"},
	{name: "paid_social", url: "https://pugandpals.example.com/x", referrer: "https://l.facebook.com/l.php", utmSource: "facebook", utmMedium: "paid_social"},
	{name: "organic_search_subdomain_ref", url: "https://pugandpals.example.com/x", referrer: "https://images.google.co.in/search"},
	{name: "organic_social_ref", url: "https://pugandpals.example.com/x", referrer: "https://news.ycombinator.com/item?id=1"},
	{name: "self_referral_www", url: "https://pugandpals.example.com/cart", referrer: "https://www.pugandpals.example.com/"},
	{name: "subdomain_not_collapsed", url: "https://app.example.com/x", referrer: "https://www.example.com/"},
	{name: "plain_referral", url: "https://pugandpals.example.com/x", referrer: "https://blog.dogfood.example.org/review"},
	{name: "email_medium", url: "https://pugandpals.example.com/x", utmSource: "lifecycle", utmMedium: "email"},
	{name: "campaign_only_unassigned", url: "https://pugandpals.example.com/x", utmCampaign: "brand"},
	{name: "video_ref", url: "https://pugandpals.example.com/x", referrer: "https://youtu.be/abc"},
	// Channel branches no other corpus row reaches (Paid Video, Display, Paid
	// Other, Affiliate), plus both directions of the cpm precedence — cpm is
	// BOTH a paid medium (matches ^.*cp.*) AND a display medium, so which
	// channel it books is decided purely by rule ORDER, and that order is the
	// one thing the Go table and the SQL multiIf must agree on exactly. Without
	// these, a single-token reordering of the SQL branches passes the parity
	// test silently and mis-books all pre-008 history.
	{name: "paid_video", url: "https://pugandpals.example.com/x", utmSource: "youtube", utmMedium: "cpv"},
	{name: "display_banner", url: "https://pugandpals.example.com/x", utmSource: "adnetwork", utmMedium: "banner"},
	// cpm + unmatched source: rule 4 (Display) wins over rule 5 (Paid Other).
	{name: "cpm_display_over_paid_other", url: "https://pugandpals.example.com/x", utmSource: "adnetwork", utmMedium: "cpm"},
	// cpm + social source: rule 2 (Paid Social) wins over rule 4 (Display).
	{name: "cpm_paid_social_over_display", url: "https://pugandpals.example.com/x", utmSource: "facebook", utmMedium: "cpm"},
	{name: "paid_other_unmatched_source", url: "https://pugandpals.example.com/x", utmSource: "partnerx", utmMedium: "cpc"},
	{name: "affiliate_medium", url: "https://pugandpals.example.com/x", utmSource: "partner", utmMedium: "affiliate"},
	// Referrers that resolve in ClickHouse's lenient domain() but NOT in Go's
	// url.Parse (which reads a schemeless string as a path). The mutation must
	// mirror Go and refuse them, so they book Unassigned rather than silently
	// splitting: Direct in live traffic, Organic Search in backfilled history.
	{name: "schemeless_referrer", url: "https://pugandpals.example.com/x", referrer: "www.google.com/search?q=x"},
	{name: "bare_host_referrer", url: "https://pugandpals.example.com/x", referrer: "google.com"},
	{name: "unparseable_referrer", url: "https://pugandpals.example.com/x", referrer: "://bad"},
	{name: "protocol_relative_referrer", url: "https://pugandpals.example.com/x", referrer: "//blog.dogfood.example.org/p"},
	{name: "android_app_referrer", url: "https://pugandpals.example.com/x", referrer: "android-app://com.google.android.gm"},
	// ClickHouse's domainWithoutWWW compares "www." case-sensitively, so an
	// uppercase WWW must be stripped by lower-then-strip to match Go — else
	// the self-referral below survives blanking and the tenant's own domain
	// becomes its top referrer for all history.
	{name: "uppercase_www_self_referral", url: "https://WWW.Shop.example.com/x", referrer: "https://WWW.Shop.example.com/y"},
	{name: "uppercase_www_external_referrer", url: "https://pugandpals.example.com/x", referrer: "https://WWW.Google.com/search"},
	// Screen dims arriving in the String variant slot (an SDK sending
	// stringValue); the second pair is unparseable and must derive nothing,
	// exactly as strconv.ParseInt failing yields 0 in the handler.
	{name: "screen_dims_as_strings", url: "https://pugandpals.example.com/x", screenWStr: "1920", screenHStr: "1080"},
	{name: "screen_dims_unparseable_strings", url: "https://pugandpals.example.com/x", screenWStr: "19x20", screenHStr: "1080"},
	// A JS SDK sending screen dims as doubles lands them in the Float64 variant
	// slot; live traffic derives a size (autoprop.String renders an integral
	// double like the Int64 slot), so the mutation must read Float64 too.
	{name: "screen_dims_as_doubles", url: "https://pugandpals.example.com/x", screenWFloat: 1920, screenHFloat: 1080},
	// A FRACTIONAL double is not an integer dimension: autoprop.String renders
	// "1920.5" and ParseInt rejects it, toInt64OrNull rejects it in SQL, so both
	// derive no $screenSize. Pins that the integral-double path above does not
	// round or truncate a fractional one into a size.
	{name: "screen_dims_fractional_double_rejected", url: "https://pugandpals.example.com/x", screenWFloat: 1920.5, screenHFloat: 1080},
	{name: "garbage_url", url: "not a url", referrer: "https://www.google.com/", locale: "EN"},
	{name: "no_url_app_event", referrer: "", locale: "zh-hans-cn", screenW: 390, screenH: 844},
	{name: "term_content_in_map", url: "https://pugandpals.example.com/x", utmTerm: "puppy harness", utmContent: "story-2", pageTitle: "Harness — Pug & Pals"},
	{name: "screen_missing_height", url: "https://pugandpals.example.com/x", screenW: 1920},
	// url.Parse splits the #fragment off BEFORE RawQuery, so a UTM living in a
	// hash route is invisible to Query(); extractURLParameter scans the whole
	// string and would read it, splitting the SPA's history into Email/Organic
	// where live traffic books Direct.
	{name: "utm_in_hash_route", url: "https://app.example.com/#/dashboard?utm_source=newsletter&utm_medium=email"},
	{name: "utm_in_fragment_after_query", url: "https://x.com/?a=1#utm_source=google"},
	// A hash route on the BARE ORIGIN — no path segment before the '#'. path()
	// excludes the fragment from what it RETURNS but still starts scanning
	// inside it, so the first '/' it finds is the fragment's: the SPA's
	// permanent $pathname reads "/dashboard" in backfilled history where live
	// traffic derives "/". The cases above all carry a '/' before the '#',
	// which is the only reason they agree.
	{name: "hash_route_no_path", url: "https://app.example.com#/dashboard"},
	{name: "hash_route_no_path_after_query", url: "https://x.com?a=1#/r"},
	// $pathname is the DECODED path (Derive uses url.URL.Path), so a literal
	// non-ASCII path and its pre-encoded twin must collapse onto ONE Pages-panel
	// row rather than fragmenting the permanent rollup.
	{name: "literal_non_ascii_path", url: "https://x.com/café"},
	{name: "encoded_non_ascii_path", url: "https://x.com/caf%C3%A9"},
	{name: "literal_non_ascii_path_nested", url: "https://shop.de/produkte/größe"},
	{name: "space_in_path", url: "https://x.com/a b"},
	// Invalid %-escapes, whose blast radius depends ENTIRELY on which URL region
	// carries them: url.Parse stores RawQuery unvalidated, so a bad escape in the
	// QUERY only drops its own key (Query() skips the pair and the rest survive);
	// the same bytes in the PATH or the FRAGMENT fail url.Parse outright and the
	// event derives nothing at all — not even hostname.
	{name: "bad_escape_in_query_value", url: "https://x.com/?utm_campaign=100%off&utm_source=google"},
	{name: "bad_escape_in_path", url: "https://shop.example.com/deals/50%-off"},
	{name: "bad_escape_in_hash_route", url: "https://app.example.com/#/search?q=100%off"},
	{name: "bad_escape_in_referrer", url: "https://pugandpals.example.com/x", referrer: "https://www.google.com/%zz"},
	// strings.TrimSpace strips the whole unicode.IsSpace set; trimBoth strips only
	// ASCII 0x20, and a surviving separator also pushes the subtag off the
	// length()=2 region re-caser ("US\n" is 3 bytes), so the residue fragments the
	// Languages panel twice over.
	{name: "locale_trailing_newline", url: "https://pugandpals.example.com/x", locale: "en-US\n"},
	{name: "locale_tab_padded", url: "https://pugandpals.example.com/x", locale: "\ten-us\t"},
	{name: "locale_blank_is_not_a_locale", url: "https://pugandpals.example.com/x", locale: "\t"},
	{name: "locale_nbsp_padded", url: "https://pugandpals.example.com/x", locale: "\u00a0en-US\u00a0"},
	// Hosts url.Parse resolves that ClickHouse's domain() does not: an IPv6
	// literal, and userinfo hiding the real host behind an embedded '@' (Go splits
	// the authority at the LAST '@', domainRFC at the first — so a URL crafted to
	// read as evil.com must still attribute to real.com).
	{name: "ipv6_literal_host", url: "https://[::1]/p"},
	{name: "userinfo_host_confusion", url: "https://user:pw@evil.com@real.com/p"},
	// Uppercase non-ASCII host/referrer: Go lowercases with Unicode-aware
	// strings.ToLower (Ü→ü), so the mutation must use lowerUTF8, not ASCII
	// lower() — which leaves Ü intact and would split one site into two
	// permanent hostname/referrer_domain rollup dim_values between live traffic
	// and backfilled history. Unreachable from a browser's location.href (hosts
	// arrive punycoded and lowercased), but a non-browser SDK can send it and
	// the mutation is a one-shot irreversible rewrite.
	{name: "non_ascii_uppercase_host", url: "https://ÜBER.example.de/x"},
	{name: "non_ascii_uppercase_referrer", url: "https://pugandpals.example.com/x", referrer: "https://ÜBER.example.de/from"},
}

func testMutation008DeriveParity(t *testing.T, ctx context.Context, ch *testutil.TestClickHouse) {
	const projectID = "proj_mut008"
	occur := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)

	batch, err := ch.Conn.PrepareBatch(ctx, pre008InsertStmt)
	if err != nil {
		t.Fatalf("prepare pre-008 batch: %v", err)
	}
	for _, row := range mutationCorpus {
		auto := map[string]chcol.Variant{}
		setStr := func(k, v string) {
			if v != "" {
				auto[k] = chcol.NewVariantWithType(v, "String")
			}
		}
		setStr("$referrer", row.referrer)
		setStr("$locale", row.locale)
		setStr("$utmTerm", row.utmTerm)
		setStr("$utmContent", row.utmContent)
		setStr("$pageTitle", row.pageTitle)
		if row.screenW != 0 {
			auto["$screenWidth"] = chcol.NewVariantWithType(row.screenW, "Int64")
		} else if row.screenWStr != "" {
			auto["$screenWidth"] = chcol.NewVariantWithType(row.screenWStr, "String")
		} else if row.screenWFloat != 0 {
			auto["$screenWidth"] = chcol.NewVariantWithType(row.screenWFloat, "Float64")
		}
		if row.screenH != 0 {
			auto["$screenHeight"] = chcol.NewVariantWithType(row.screenH, "Int64")
		} else if row.screenHStr != "" {
			auto["$screenHeight"] = chcol.NewVariantWithType(row.screenHStr, "String")
		} else if row.screenHFloat != 0 {
			auto["$screenHeight"] = chcol.NewVariantWithType(row.screenHFloat, "Float64")
		}
		if err := batch.Append(
			uuid.New().String(), projectID, row.name, "page_view",
			auto, map[string]chcol.Variant(nil),
			row.url, row.utmSource, row.utmMedium, row.utmCampaign,
			occur, uuid.NewString(),
		); err != nil {
			t.Fatalf("append pre-008 row %s: %v", row.name, err)
		}
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("send pre-008 batch: %v", err)
	}

	mutation := extractMigrationStatement(t, "008_add_web_analytics_columns.sql",
		`(?s)ALTER TABLE events UPDATE.*?WHERE 1 SETTINGS mutations_sync = 2`)
	// Run twice: the second run pins the idempotency guards (a re-derivation
	// that changed anything would fail the comparisons below just as a wrong
	// first derivation would).
	for range 2 {
		if err := ch.Conn.Exec(ctx, mutation); err != nil {
			t.Fatalf("exec 008 mutation: %v", err)
		}
	}

	rows, err := ch.Conn.Query(ctx, `SELECT distinct_id, pathname, hostname, referrer, referrer_domain, channel, locale, screen_size, utm_source, utm_medium, utm_campaign, utm_term, utm_content, page_title FROM events WHERE project_id = ?`, projectID)
	if err != nil {
		t.Fatalf("read mutated rows: %v", err)
	}
	defer func() { _ = rows.Close() }()

	type mutated struct {
		pathname, hostname, referrer, referrerDomain, channel, locale, screenSize string
		utmSource, utmMedium, utmCampaign, utmTerm, utmContent, pageTitle         string
	}
	got := map[string]mutated{}
	for rows.Next() {
		var name string
		var m mutated
		if err := rows.Scan(&name, &m.pathname, &m.hostname, &m.referrer, &m.referrerDomain, &m.channel, &m.locale, &m.screenSize,
			&m.utmSource, &m.utmMedium, &m.utmCampaign, &m.utmTerm, &m.utmContent, &m.pageTitle); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[name] = m
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(got) != len(mutationCorpus) {
		t.Fatalf("read back %d rows, corpus has %d", len(got), len(mutationCorpus))
	}

	for _, row := range mutationCorpus {
		out := attribution.Derive(row.deriveInput())
		m, ok := got[row.name]
		if !ok {
			t.Errorf("%s: row missing after mutation", row.name)
			continue
		}
		want := mutated{
			pathname:       out.Pathname,
			hostname:       out.Hostname,
			referrer:       row.referrer,
			referrerDomain: out.ReferrerDomain,
			channel:        out.Channel,
			locale:         out.Locale,
			screenSize:     out.ScreenSize,
			utmSource:      out.UTMSource,
			utmMedium:      out.UTMMedium,
			utmCampaign:    out.UTMCampaign,
			utmTerm:        out.UTMTerm,
			utmContent:     out.UTMContent,
			pageTitle:      row.pageTitle,
		}
		if m != want {
			t.Errorf("%s: mutation vs Derive mismatch:\nmutation: %+v\nderive:   %+v", row.name, m, want)
		}
	}

	// $locale is the one promoted key the mutation TRANSFORMS rather than
	// copies, so leaving the raw map entry behind would outrank the normalized
	// column on read (MergeIntoAutoProperties is map-wins): the events API would
	// answer "en-us" where insights answer "en-US". The mutation drops that key
	// and ONLY that key — the copied-verbatim residue ($referrer/$utmTerm/…)
	// merges to an identical value and is left alone.
	var localeResidue uint64
	if err := ch.Conn.QueryRow(ctx,
		`SELECT count() FROM events WHERE project_id = ? AND mapContains(auto_properties, '$locale')`,
		projectID,
	).Scan(&localeResidue); err != nil {
		t.Fatalf("count $locale residue: %v", err)
	}
	if localeResidue != 0 {
		t.Errorf("%d rows still carry auto_properties['$locale'] after the mutation; the un-normalized residue would win the map-wins read merge over the normalized locale column", localeResidue)
	}

	var referrerResidue uint64
	if err := ch.Conn.QueryRow(ctx,
		`SELECT count() FROM events WHERE project_id = ? AND mapContains(auto_properties, '$referrer')`,
		projectID,
	).Scan(&referrerResidue); err != nil {
		t.Fatalf("count $referrer residue: %v", err)
	}
	if referrerResidue == 0 {
		t.Error("the mutation cleared more than '$locale' from auto_properties — the other promoted keys are copied verbatim and must stay, or this becomes an unreviewed map rewrite")
	}
}

// ---------------------------------------------------------------------------
// 010 partial-column backfill merge identity
// ---------------------------------------------------------------------------

func testSessionRollupPartialInsertMergeIdentity(t *testing.T, ctx context.Context, ch *testutil.TestClickHouse) {
	const projectID = "proj_mergeid"
	sessionID := "00000000-0000-0000-0000-00000000ae01"
	base := time.Date(2024, 2, 1, 10, 0, 0, 0, time.UTC)

	// One two-event session flowing through the live MV (full states).
	for i, path := range []string{"/landing", "/checkout"} {
		if err := insertWebEvent(ctx, ch, projectID, "ida", sessionID, base.Add(time.Duration(i)*5*time.Minute), webProps{
			pathname: path, channel: "Organic Search", referrerDomain: "google.com", url: "https://pugandpals.example.com" + path,
		}); err != nil {
			t.Fatalf("seed session event: %v", err)
		}
	}

	type snapshot struct {
		count           uint64
		durationSeconds int64
		entryURL        string
		entryPathname   string
		exitPathname    string
	}
	read := func() snapshot {
		var s snapshot
		if err := ch.Conn.QueryRow(ctx, `
			SELECT
				countMerge(event_count_state),
				dateDiff('second', minMerge(start_state), maxMerge(end_state)),
				argMinMerge(entry_url_state),
				argMinMerge(entry_pathname_state),
				argMaxMerge(exit_pathname_state)
			FROM dashboard_session_rollup
			WHERE project_id = ? AND kind = '' AND session_id = toUUID(?)`,
			projectID, sessionID,
		).Scan(&s.count, &s.durationSeconds, &s.entryURL, &s.entryPathname, &s.exitPathname); err != nil {
			t.Fatalf("read session rollup: %v", err)
		}
		return s
	}

	before := read()
	if before.count != 2 || before.durationSeconds != 300 {
		t.Fatalf("baseline session wrong: %+v (want 2 events, 300s)", before)
	}
	if before.entryPathname != "/landing" || before.exitPathname != "/checkout" {
		t.Fatalf("baseline entry/exit pathname wrong: %+v", before)
	}

	// Re-run migration 010's partial-column backfill INSERT verbatim. Omitted
	// AggregateFunction columns must take the empty state — the merge identity
	// — so count/duration/entry_url stay EXACTLY as before; the argMin/argMax
	// states are idempotent under the duplicate merge.
	backfill := extractMigrationStatement(t, "010_extend_dashboard_session_rollup.sql",
		`(?s)INSERT INTO dashboard_session_rollup \(.*?GROUP BY project_id, kind, session_id`)
	if err := ch.Conn.Exec(ctx, backfill); err != nil {
		t.Fatalf("exec 010 partial backfill: %v", err)
	}

	after := read()
	if after != before {
		t.Errorf("partial-column insert corrupted merged state:\nbefore: %+v\nafter:  %+v", before, after)
	}
}

// testBackfill009Rerunnable pins migration 009's delta backfill as re-runnable.
// cnt is SimpleAggregateFunction(sum), so the INSERT alone is NOT idempotent:
// an INSERT ... SELECT that dies partway (max_execution_time, memory) cannot
// roll back and leaves its parts behind, and goose records no version — so the
// natural `pug clickhouse migrate` retry lays a second full copy on top and
// doubles cnt for every new dim forever (UNIQUE_USERS stays right, so it reads
// as plausible rather than broken). The DELETE prelude is the only thing making
// that retry safe; without it this test fails with cnt exactly doubled.
func testBackfill009Rerunnable(t *testing.T, ctx context.Context, ch *testutil.TestClickHouse) {
	const projectID = "proj_rerun009"
	occur := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	for _, path := range []string{"/a", "/b", "/b"} {
		if err := insertWebEvent(ctx, ch, projectID, "ida", uuid.NewString(), occur, webProps{
			url: "https://pugandpals.example.com" + path, pathname: path,
			channel: "Organic Search", referrerDomain: "google.com", locale: "en-US",
		}); err != nil {
			t.Fatalf("seed event: %v", err)
		}
	}

	read := func() map[string]uint64 {
		rows, err := ch.Conn.Query(ctx, `
			SELECT dim_name, sum(cnt)
			FROM dashboard_event_rollup_daily
			WHERE project_id = ?
			GROUP BY dim_name`, projectID)
		if err != nil {
			t.Fatalf("read event rollup: %v", err)
		}
		defer func() { _ = rows.Close() }()
		out := make(map[string]uint64)
		for rows.Next() {
			var name string
			var cnt uint64
			if err := rows.Scan(&name, &cnt); err != nil {
				t.Fatalf("scan event rollup: %v", err)
			}
			out[name] = cnt
		}
		return out
	}

	before := read()
	for _, dim := range []string{"$pathname", "$channel", "$__total__"} {
		if before[dim] != 3 {
			t.Fatalf("baseline %s cnt = %d, want 3: %+v", dim, before[dim], before)
		}
	}

	// Replay 009's guard + backfill, exactly as a retried migrate would — with
	// one post-011 adaptation: 009's INSERT is positional (no column list),
	// which goose can only ever execute against the pre-011 seven-column table
	// (a 009 retry always precedes 011 in migration order), but THIS database
	// is fully migrated and carries 011's `cookieless` key column. Naming
	// 009's seven columns pins the shipped statement's idempotency against the
	// live schema; the omitted flag takes type-default 0, correct for this
	// all-consented corpus. The post-011 MANUAL repair must instead use the
	// cookieless-aware statement in web-analytics.md's runbook.
	for _, step := range []struct{ pattern, columnList string }{
		{`(?s)ALTER TABLE dashboard_event_rollup_daily DELETE\s+WHERE dim_name IN \(.*?SETTINGS mutations_sync = 2`, ""},
		{`(?s)INSERT INTO dashboard_event_rollup_daily\nSELECT.*?GROUP BY project_id, day, kind, dim_name, dim_value`,
			"(project_id, day, kind, dim_name, dim_value, cnt, uniq_state)"},
	} {
		stmt := extractMigrationStatement(t, "009_extend_dashboard_event_rollup.sql", step.pattern)
		if step.columnList != "" {
			stmt = strings.Replace(stmt,
				"INSERT INTO dashboard_event_rollup_daily\n",
				"INSERT INTO dashboard_event_rollup_daily "+step.columnList+"\n", 1)
		}
		if err := ch.Conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("exec 009 backfill step: %v", err)
		}
	}

	if after := read(); !reflect.DeepEqual(before, after) {
		t.Errorf("009 delta backfill is not re-runnable — cnt changed on replay:\nbefore: %+v\nafter:  %+v", before, after)
	}
}

// extractMigrationStatement pulls one statement out of a migration file by
// regex, so integration tests execute the SHIPPED SQL rather than a copy that
// could drift.
func extractMigrationStatement(t *testing.T, file, pattern string) string {
	t.Helper()
	data, err := os.ReadFile("../../../schema/clickhouse/migrations/" + file)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	m := regexp.MustCompile(pattern).FindString(string(data))
	if m == "" {
		t.Fatalf("pattern %q not found in %s", pattern, file)
	}
	return m
}

// ---------------------------------------------------------------------------
// Web-analytics rollup parity: seeds and helpers
// ---------------------------------------------------------------------------

type webProps struct {
	url            string
	pathname       string
	channel        string
	referrerDomain string
	locale         string
	screenSize     string
	utmTerm        string
}

func (p webProps) variantMap() map[string]chcol.Variant {
	m := map[string]string{}
	set := func(k, v string) {
		if v != "" {
			m[k] = v
		}
	}
	set("$url", p.url)
	set("$pathname", p.pathname)
	set("$channel", p.channel)
	set("$referrerDomain", p.referrerDomain)
	set("$locale", p.locale)
	set("$screenSize", p.screenSize)
	set("$utmTerm", p.utmTerm)
	return variantStringMap(m)
}

// insertWebEvent inserts one page_view with web-analytics promoted properties
// and an explicit session id (PrepareEventInsertArgs splits the promoted keys
// into their dedicated columns, which is what the MVs roll up).
func insertWebEvent(ctx context.Context, ch *testutil.TestClickHouse, projectID, distinctID, sessionID string, occur time.Time, p webProps) error {
	return insertWebEventKind(ctx, ch, projectID, distinctID, sessionID, "page_view", occur, p)
}

// insertWebEventKind is insertWebEvent for a non-page_view kind. The session
// rollup keys on kind, so a project seeded with page_view alone leaves its
// empty-kind and kind='page_view' rows carrying identical state — and every
// kind-scoped assertion passes without ever discriminating the scope.
func insertWebEventKind(ctx context.Context, ch *testutil.TestClickHouse, projectID, distinctID, sessionID, kind string, occur time.Time, p webProps) error {
	batch, err := ch.Conn.PrepareBatch(ctx, chq.EventsInsertStmt)
	if err != nil {
		return err
	}
	if err := batch.Append(chq.PrepareEventInsertArgs(
		uuid.New().String(), projectID, distinctID, kind,
		p.variantMap(), nil,
		occur, sessionID,
	)...); err != nil {
		return err
	}
	return batch.Send()
}

// seedWebEventGrain: deterministic event-grain rows for trends/top-K parity.
// Day 1: alice /home Organic Search, bob /pricing Direct, carol /home Paid Search.
// Day 2: alice /docs Organic Social, bob /home Direct.
// Day 3: alice /home Organic Search.
//
// screenSize and utmTerm carry two distinct values apiece so a breakdown over
// either is discriminating rather than single-valued.
func seedWebEventGrain(t *testing.T, ctx context.Context, ch *testutil.TestClickHouse, projectID string) {
	t.Helper()
	rows := []struct {
		day        int
		user       string
		pathname   string
		channel    string
		refDom     string
		screenSize string
		utmTerm    string
	}{
		{1, "alice", "/home", "Organic Search", "google.com", "1920x1080", "dog treats"},
		{1, "bob", "/pricing", "Direct", "", "390x844", "dog treats"},
		{1, "carol", "/home", "Paid Search", "google.com", "1920x1080", "puppy harness"},
		{2, "alice", "/docs", "Organic Social", "reddit.com", "1920x1080", "puppy harness"},
		{2, "bob", "/home", "Direct", "", "390x844", "dog treats"},
		{3, "alice", "/home", "Organic Search", "google.com", "1920x1080", "dog treats"},
	}
	for _, r := range rows {
		occur := time.Date(2024, 1, r.day, 12, 0, 0, 0, time.UTC)
		if err := insertWebEvent(ctx, ch, projectID, r.user, uuid.NewString(), occur, webProps{
			url: "https://pugandpals.example.com" + r.pathname, pathname: r.pathname,
			channel: r.channel, referrerDomain: r.refDom, locale: "en-US",
			screenSize: r.screenSize, utmTerm: r.utmTerm,
		}); err != nil {
			t.Fatalf("seed web event: %v", err)
		}
	}
}

// seedWebSessionGrain: three sessions for the session-grain panels.
// S1 alice Jan1: /landing (Organic Search, google.com) 10:00 → /checkout (Direct) 10:05.
// S2 bob   Jan1: /home (Direct) 11:00 — bounce.
// S3 carol Jan2: /a (Organic Social, reddit.com) 9:00 → /b 9:10 → /c 9:30.
func seedWebSessionGrain(t *testing.T, ctx context.Context, ch *testutil.TestClickHouse, projectID string) {
	t.Helper()
	jan := func(day, hour, minute int) time.Time {
		return time.Date(2024, 1, day, hour, minute, 0, 0, time.UTC)
	}
	sessions := map[string]string{
		"S1": "00000000-0000-0000-0000-00000000f001",
		"S2": "00000000-0000-0000-0000-00000000f002",
		"S3": "00000000-0000-0000-0000-00000000f003",
	}
	rows := []struct {
		session, user string
		at            time.Time
		p             webProps
	}{
		{"S1", "alice", jan(1, 10, 0), webProps{pathname: "/landing", channel: "Organic Search", referrerDomain: "google.com"}},
		{"S1", "alice", jan(1, 10, 5), webProps{pathname: "/checkout", channel: "Direct"}},
		{"S2", "bob", jan(1, 11, 0), webProps{pathname: "/home", channel: "Direct"}},
		{"S3", "carol", jan(2, 9, 0), webProps{pathname: "/a", channel: "Organic Social", referrerDomain: "reddit.com"}},
		{"S3", "carol", jan(2, 9, 10), webProps{pathname: "/b", channel: "Direct"}},
		{"S3", "carol", jan(2, 9, 30), webProps{pathname: "/c", channel: "Direct"}},
	}
	for _, r := range rows {
		r.p.url = "https://pugandpals.example.com" + r.p.pathname
		if err := insertWebEvent(ctx, ch, projectID, r.user, sessions[r.session], r.at, r.p); err != nil {
			t.Fatalf("seed web session event: %v", err)
		}
	}
}

// seedWebMixedKindGrain: one session that opens with a URL-less non-web event
// and then pageviews, so the empty-kind and kind='page_view' session-rollup rows
// carry DIFFERENT entry states and event counts.
// S4 dave Apr1: identify (no url) 8:00 → /landing 8:01 → /checkout 8:20.
func seedWebMixedKindGrain(t *testing.T, ctx context.Context, ch *testutil.TestClickHouse, projectID string) {
	t.Helper()
	const sessionID = "00000000-0000-0000-0000-00000000f004"
	apr := func(hour, minute int) time.Time {
		return time.Date(2024, 4, 1, hour, minute, 0, 0, time.UTC)
	}
	if err := insertWebEventKind(ctx, ch, projectID, "dave", sessionID, "identify", apr(8, 0), webProps{}); err != nil {
		t.Fatalf("seed non-web session event: %v", err)
	}
	for _, r := range []struct {
		at       time.Time
		pathname string
		channel  string
		refDom   string
	}{
		{apr(8, 1), "/landing", "Organic Search", "google.com"},
		{apr(8, 20), "/checkout", "Direct", ""},
	} {
		if err := insertWebEvent(ctx, ch, projectID, "dave", sessionID, r.at, webProps{
			url: "https://pugandpals.example.com" + r.pathname, pathname: r.pathname,
			channel: r.channel, referrerDomain: r.refDom,
		}); err != nil {
			t.Fatalf("seed web session event: %v", err)
		}
	}
}

func webWindow() *commonv1.TimeRange {
	return &commonv1.TimeRange{
		From: timestamppb.New(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
		To:   timestamppb.New(time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC)),
	}
}

func webMixedWindow() *commonv1.TimeRange {
	return &commonv1.TimeRange{
		From: timestamppb.New(time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC)),
		To:   timestamppb.New(time.Date(2024, 4, 2, 0, 0, 0, 0, time.UTC)),
	}
}

func webTrendsReq(agg insightsv1.AggregationType, kind, breakdown string) *insightsv1.QueryRequest {
	spec := &insightsv1.InsightQuerySpec{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
		Events: []*insightsv1.EventQuery{
			{Event: &commonv1.EventFilter{Kind: proto.String(kind)}, Aggregation: agg.Enum()},
		},
	}
	if breakdown != "" {
		spec.Breakdowns = []*insightsv1.Breakdown{{Property: proto.String(breakdown)}}
	}
	return &insightsv1.QueryRequest{Spec: spec, TimeRange: webWindow(), Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum()}
}

func webSessionTrendsReq(metric insightsv1.SessionMetric, breakdown, scopeKind string) *insightsv1.QueryRequest {
	session := &insightsv1.SessionQuery{Metric: metric.Enum()}
	if scopeKind != "" {
		session.Scope = &commonv1.EventFilter{Kind: proto.String(scopeKind)}
	}
	spec := &insightsv1.InsightQuerySpec{
		InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
		Session:     session,
	}
	if breakdown != "" {
		spec.Breakdowns = []*insightsv1.Breakdown{{Property: proto.String(breakdown)}}
	}
	return &insightsv1.QueryRequest{Spec: spec, TimeRange: webWindow(), Granularity: insightsv1.Granularity_GRANULARITY_DAY.Enum()}
}

// webMixedSessionTrendsReq is webSessionTrendsReq over the mixed-kind seed's
// own window, which is deliberately disjoint from the Jan sessions'.
func webMixedSessionTrendsReq(metric insightsv1.SessionMetric, breakdown, scopeKind string) *insightsv1.QueryRequest {
	req := webSessionTrendsReq(metric, breakdown, scopeKind)
	req.TimeRange = webMixedWindow()
	return req
}

// flattenTrendsRespBy mirrors flattenTrendsResp for an arbitrary breakdown
// property (the shared helper hardcodes $country; two distinct breakdown
// values must never collapse onto one key, or parity checks go vacuous).
func flattenTrendsRespBy(resp *insightsv1.QueryResponse, prop string) map[string]float64 {
	out := map[string]float64{}
	for _, s := range resp.GetTrends().GetSeries() {
		bd := s.GetBreakdown()[prop]
		for _, p := range s.GetPoints() {
			out[s.GetEventKind()+"|"+bd+"|"+p.GetTime().AsTime().Format("2006-01-02")] = p.GetValue()
		}
	}
	return out
}

// assertTrendsParityBy asserts rollup==raw and returns the flattened rollup
// result keyed "kind|breakdown|date" for sanity assertions.
func assertTrendsParityBy(t *testing.T, ctx context.Context, executor *insights.Executor, projectID string, req *insightsv1.QueryRequest, prop string) map[string]float64 {
	t.Helper()
	resp, err := insights.ExecuteQuery(ctx, executor, projectID, req, time.Now())
	if err != nil {
		t.Fatalf("ExecuteQuery (rollup): %v", err)
	}
	rollup := flattenTrendsRespBy(resp, prop)

	rawQ, err := insights.BuildTrendsQuery(req, projectID)
	if err != nil {
		t.Fatalf("BuildTrendsQuery (raw): %v", err)
	}
	rawRows, err := executor.QueryTrends(ctx, projectID, rawQ)
	if err != nil {
		t.Fatalf("QueryTrends (raw): %v", err)
	}
	series, err := insights.GroupSeries(ctx, rawRows, rawQ.Properties(), rawQ.BreakdownLimit())
	if err != nil {
		t.Fatalf("GroupSeries (raw): %v", err)
	}
	raw := flattenTrendsRespBy(&insightsv1.QueryResponse{
		Result: &insightsv1.QueryResponse_Trends{Trends: &insightsv1.TrendsResult{Series: series}},
	}, prop)

	if !reflect.DeepEqual(rollup, raw) {
		t.Errorf("rollup vs raw mismatch:\nrollup=%v\nraw=%v", rollup, raw)
	}
	if len(rollup) == 0 {
		t.Error("empty result — seed/window mismatch would make this parity check vacuous")
	}
	return rollup
}

// assertSessionTrendsParityBy mirrors assertSessionTrendsParity for an
// arbitrary breakdown property and returns the flattened rollup result keyed
// "breakdown|date" for sanity assertions.
func assertSessionTrendsParityBy(t *testing.T, ctx context.Context, executor *insights.Executor, projectID string, req *insightsv1.QueryRequest, prop string) map[string]float64 {
	t.Helper()
	flatten := func(resp *insightsv1.QueryResponse) map[string]float64 {
		out := map[string]float64{}
		for _, s := range resp.GetTrends().GetSeries() {
			bd := ""
			if prop != "" {
				bd = s.GetBreakdown()[prop]
			}
			for _, p := range s.GetPoints() {
				out[bd+"|"+p.GetTime().AsTime().Format("2006-01-02")] = p.GetValue()
			}
		}
		return out
	}

	resp, err := insights.ExecuteQuery(ctx, executor, projectID, req, time.Now())
	if err != nil {
		t.Fatalf("ExecuteQuery (rollup): %v", err)
	}
	rollup := flatten(resp)

	rawQ, err := insights.BuildSessionTrendsQuery(req, projectID)
	if err != nil {
		t.Fatalf("BuildSessionTrendsQuery (raw): %v", err)
	}
	rawRows, err := executor.QueryTrends(ctx, projectID, rawQ)
	if err != nil {
		t.Fatalf("QueryTrends (raw): %v", err)
	}
	series, err := insights.GroupSeries(ctx, rawRows, rawQ.Properties(), rawQ.BreakdownLimit())
	if err != nil {
		t.Fatalf("GroupSeries (raw): %v", err)
	}
	raw := flatten(&insightsv1.QueryResponse{
		Result: &insightsv1.QueryResponse_Trends{Trends: &insightsv1.TrendsResult{Series: series}},
	})

	if !reflect.DeepEqual(rollup, raw) {
		t.Errorf("session trends rollup vs raw mismatch:\nrollup=%v\nraw=%v", rollup, raw)
	}
	if len(rollup) == 0 {
		t.Error("empty result — seed/window mismatch would make this parity check vacuous")
	}
	return rollup
}
