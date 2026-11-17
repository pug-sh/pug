package insights_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/pug-sh/pug/internal/core/insights"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/testutil"
)

// toStartOfWeek and toStartOfMonth return a Date, which the driver reads as the server's midnight, not
// the project's: west of the server, a month then reads as the one before.
func TestIntegration_WeekAndMonthBucketsStartAtTheProjectsMidnight(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ch := testutil.SetupClickHouse(t)
	ctx := context.Background()
	executor := insights.NewExecutor(ch.Conn)
	const projectID = "proj_week_month_zone"

	// Tuesday 12 March 2024, 17:30 in Kolkata.
	at := time.Date(2024, 3, 12, 12, 0, 0, 0, time.UTC)
	if err := insertAutoEvent(ctx, ch.Conn, projectID, uuid.NewString(), "page_view", "anon-1", at, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}

	kolkata, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name        string
		granularity insightsv1.Granularity
		want        time.Time
	}{
		{"week", insightsv1.Granularity_GRANULARITY_WEEK, time.Date(2024, 3, 10, 0, 0, 0, 0, kolkata)},
		{"month", insightsv1.Granularity_GRANULARITY_MONTH, time.Date(2024, 3, 1, 0, 0, 0, 0, kolkata)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, err := insights.ExecuteQuery(ctx, executor, projectID, &insightsv1.QueryRequest{
				Spec: &insightsv1.InsightQuerySpec{
					InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
					Events:      []*insightsv1.EventQuery{{Event: &commonv1.EventFilter{Kind: proto.String("page_view")}}},
				},
				TimeRange: &commonv1.TimeRange{
					From: timestamppb.New(time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)),
					To:   timestamppb.New(time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC)),
				},
				Granularity: c.granularity.Enum(),
				Timezone:    proto.String("Asia/Kolkata"),
			}, time.Now())
			if err != nil {
				t.Fatalf("ExecuteQuery: %v", err)
			}

			series := resp.GetTrends().GetSeries()
			if len(series) != 1 || len(series[0].GetPoints()) != 1 {
				t.Fatalf("want one series with one point, got %v", series)
			}
			if got := series[0].GetPoints()[0].GetTime().AsTime(); !got.Equal(c.want) {
				t.Errorf("bucket = %s, want %s", got.UTC().Format(time.RFC3339), c.want.UTC().Format(time.RFC3339))
			}
		})
	}
}
