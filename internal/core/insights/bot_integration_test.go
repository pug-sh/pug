package insights_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/chcol"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"

	"github.com/pug-sh/pug/internal/core/insights"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/testutil"
)

// TestIntegrationBotTagging proves migration 012 end to end through real
// ClickHouse: the promoted bot columns round-trip through the insert path, both
// rollups key on bot, and an unpredicated ExecuteQuery still merges both key
// values (the toggle that would exclude them does not exist yet).
func TestIntegrationBotTagging(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()
	const projectID = "proj_bot"

	web := chcol.NewVariantWithType("web", "String")
	human := map[string]chcol.Variant{"$platform": web}
	bot := map[string]chcol.Variant{
		"$platform":   web,
		"$bot":        chcol.NewVariantWithType(true, "Bool"),
		"$bot_reason": chcol.NewVariantWithType("HeadlessChrome", "String"),
	}
	day := time.Date(2024, 1, 2, 12, 0, 0, 0, time.UTC)
	for i, v := range []struct {
		id    string
		props map[string]chcol.Variant
	}{{"anon-u1", human}, {"anon-u2", human}, {"anon-monitor", bot}} {
		if err := insertAutoEvent(ctx, ch.Conn, projectID, uuid.NewString(), "page_view", v.id,
			day.Add(time.Duration(i)*time.Minute), v.props); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	t.Run("events_columns_round_trip", func(t *testing.T) {
		rows, err := ch.Conn.Query(ctx,
			"SELECT distinct_id, bot, bot_reason FROM events WHERE project_id = ? ORDER BY distinct_id", projectID)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		got := map[string]string{}
		for rows.Next() {
			var id, reason string
			var flag bool
			if err := rows.Scan(&id, &flag, &reason); err != nil {
				t.Fatal(err)
			}
			if flag {
				got[id] = reason
			} else {
				got[id] = "human:" + reason
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"anon-u1": "human:", "anon-u2": "human:", "anon-monitor": "HeadlessChrome"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("events bot columns = %v, want %v", got, want)
		}
	})

	t.Run("event_rollup_keyed_by_bot", func(t *testing.T) {
		got := countByBot(ctx, t, ch.Conn,
			"SELECT bot, sum(cnt) FROM dashboard_event_rollup_daily WHERE project_id = ? AND dim_name = '$__total__' GROUP BY bot", projectID)
		if want := map[uint8]uint64{0: 2, 1: 1}; !reflect.DeepEqual(got, want) {
			t.Errorf("$__total__ cnt by bot = %v, want %v", got, want)
		}
	})

	t.Run("session_rollup_keyed_by_bot", func(t *testing.T) {
		for _, kind := range []string{"", "page_view"} {
			got := countByBot(ctx, t, ch.Conn,
				"SELECT bot, count(DISTINCT session_id) FROM dashboard_session_rollup WHERE project_id = ? AND kind = ? GROUP BY bot", projectID, kind)
			if want := map[uint8]uint64{0: 2, 1: 1}; !reflect.DeepEqual(got, want) {
				t.Errorf("kind=%q sessions by bot = %v, want %v", kind, got, want)
			}
		}
	})

	t.Run("execute_query_merges_both_key_values", func(t *testing.T) {
		executor := insights.NewExecutor(ch.Conn)
		resp, err := insights.ExecuteQuery(ctx, executor, projectID,
			clTrendsReq(insightsv1.AggregationType_AGGREGATION_TYPE_TOTAL, false), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if got := sumTrendsValues(resp); got != 3 {
			t.Errorf("TOTAL = %v, want 3 (no bot predicate exists yet, so both key values sum)", got)
		}
	})
}

func countByBot(ctx context.Context, t *testing.T, conn driver.Conn, query string, args ...any) map[uint8]uint64 {
	t.Helper()
	rows, err := conn.Query(ctx, query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[uint8]uint64{}
	for rows.Next() {
		var flag uint8
		var n uint64
		if err := rows.Scan(&flag, &n); err != nil {
			t.Fatal(err)
		}
		out[flag] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
