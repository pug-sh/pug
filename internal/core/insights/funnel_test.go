package insights_test

import (
	"context"
	"testing"
	"time"

	"github.com/fivebitsio/cotton/internal/core/insights"
)

var ctx = context.Background()

func TestComputeFunnelTiming(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(1 * time.Hour)
	t2 := t0.Add(3 * time.Hour)

	users := []insights.FunnelUserEvents{
		{
			// User completes all 3 steps: signup(t0) → cart(t1) → purchase(t2)
			DistinctID:  "user-1",
			Times:       []time.Time{t0, t1, t2},
			StepMatches: []int64{0, 1, 2},
		},
		{
			// User completes only step 0 and 1
			DistinctID:  "user-2",
			Times:       []time.Time{t0, t1},
			StepMatches: []int64{0, 1},
		},
		{
			// User completes only step 0
			DistinctID:  "user-3",
			Times:       []time.Time{t0},
			StepMatches: []int64{0},
		},
	}

	kinds := []string{"signup", "cart", "purchase"}
	rows, err := insights.ComputeFunnelTiming(ctx, users, kinds, 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}

	// Step 0: all 3 users
	if rows[0].Value != 3 {
		t.Errorf("step 0 count: got %v, want 3", rows[0].Value)
	}
	if rows[0].EventKind != "signup" {
		t.Errorf("step 0 kind: got %v, want signup", rows[0].EventKind)
	}
	if rows[0].AvgConvertSeconds != 0 {
		t.Errorf("step 0 timing should be 0, got %v", rows[0].AvgConvertSeconds)
	}

	// Step 1: user-1 and user-2 (both 1 hour from step 0)
	if rows[1].Value != 2 {
		t.Errorf("step 1 count: got %v, want 2", rows[1].Value)
	}
	if rows[1].AvgConvertSeconds != 3600 {
		t.Errorf("step 1 avg time: got %v, want 3600", rows[1].AvgConvertSeconds)
	}

	// Step 2: only user-1 (2 hours from step 1)
	if rows[2].Value != 1 {
		t.Errorf("step 2 count: got %v, want 1", rows[2].Value)
	}
	if rows[2].AvgConvertSeconds != 7200 {
		t.Errorf("step 2 avg time: got %v, want 7200", rows[2].AvgConvertSeconds)
	}
}

func TestComputeFunnelTiming_ConversionWindow(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(30 * time.Minute)
	t2 := t0.Add(2 * time.Hour) // exceeds 1-hour window from t0

	users := []insights.FunnelUserEvents{
		{
			DistinctID:  "user-1",
			Times:       []time.Time{t0, t1, t2},
			StepMatches: []int64{0, 1, 2},
		},
	}

	rows, err := insights.ComputeFunnelTiming(ctx, users, []string{"a", "b", "c"}, 3600, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Step 0 and 1 within window, step 2 exceeds it
	if rows[0].Value != 1 {
		t.Errorf("step 0: got %v, want 1", rows[0].Value)
	}
	if rows[1].Value != 1 {
		t.Errorf("step 1: got %v, want 1", rows[1].Value)
	}
	if rows[2].Value != 0 {
		t.Errorf("step 2: got %v, want 0 (outside window)", rows[2].Value)
	}
}

func TestComputeFunnelTiming_WindowExactBoundary(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(1 * time.Hour) // exactly at window boundary

	users := []insights.FunnelUserEvents{
		{
			DistinctID:  "user-1",
			Times:       []time.Time{t0, t1},
			StepMatches: []int64{0, 1},
		},
	}

	// Window is 3600s. Step 1 is exactly 3600s after step 0.
	// windowFunnel uses <=, our Go logic uses > (strictly greater), so exact boundary is included.
	rows, err := insights.ComputeFunnelTiming(ctx, users, []string{"a", "b"}, 3600, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if rows[1].Value != 1 {
		t.Errorf("step 1 at exact boundary: got %v, want 1 (should be included)", rows[1].Value)
	}
}

func TestComputeFunnelTiming_GreedyOutOfOrderSteps(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(1 * time.Hour)
	t2 := t0.Add(2 * time.Hour)

	users := []insights.FunnelUserEvents{
		{
			// Events: [step1_match, step0_match, step1_match]
			// Greedy walk: skip step1@t0, match step0@t1, match step1@t2
			DistinctID:  "user-1",
			Times:       []time.Time{t0, t1, t2},
			StepMatches: []int64{1, 0, 1},
		},
	}

	rows, err := insights.ComputeFunnelTiming(ctx, users, []string{"a", "b"}, 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if rows[0].Value != 1 {
		t.Errorf("step 0: got %v, want 1", rows[0].Value)
	}
	if rows[1].Value != 1 {
		t.Errorf("step 1: got %v, want 1", rows[1].Value)
	}
	// Time from step 0 (t1) to step 1 (t2) = 1 hour
	if rows[1].AvgConvertSeconds != 3600 {
		t.Errorf("step 1 avg time: got %v, want 3600", rows[1].AvgConvertSeconds)
	}
}

func TestComputeFunnelTiming_NoUsers(t *testing.T) {
	rows, err := insights.ComputeFunnelTiming(ctx, nil, []string{"a", "b"}, 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if rows[0].Value != 0 || rows[1].Value != 0 {
		t.Errorf("expected zero counts for no users, got %v, %v", rows[0].Value, rows[1].Value)
	}
}

func TestComputeFunnelTiming_NoUsersWithBreakdowns(t *testing.T) {
	rows, err := insights.ComputeFunnelTiming(ctx, nil, []string{"a", "b"}, 0, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if rows[0].Value != 0 || rows[1].Value != 0 {
		t.Errorf("expected zero counts, got %v, %v", rows[0].Value, rows[1].Value)
	}
	// With numBreakdowns=1, the sentinel path produces a single empty breakdown string.
	if len(rows[0].Breakdowns) != 1 || rows[0].Breakdowns[0] != "" {
		t.Errorf("expected single empty breakdown, got %v", rows[0].Breakdowns)
	}
}

func TestComputeFunnelTiming_EmptyKindsReturnsError(t *testing.T) {
	if _, err := insights.ComputeFunnelTiming(ctx, nil, nil, 0, 0); err == nil {
		t.Fatal("expected error for empty kinds")
	}
}

func TestComputeFunnelTiming_MismatchedArraysReturnsError(t *testing.T) {
	users := []insights.FunnelUserEvents{
		{
			DistinctID:  "user-1",
			Times:       []time.Time{time.Now(), time.Now()},
			StepMatches: []int64{0}, // length 1 vs 2
		},
	}

	if _, err := insights.ComputeFunnelTiming(ctx, users, []string{"a", "b"}, 0, 0); err == nil {
		t.Fatal("expected error for mismatched array lengths")
	}
}

func TestComputeFunnelTiming_WithBreakdowns(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(1 * time.Hour)
	t2 := t0.Add(2 * time.Hour)

	users := []insights.FunnelUserEvents{
		// US users: both complete both steps
		{DistinctID: "user-1", Times: []time.Time{t0, t1}, StepMatches: []int64{0, 1}, Breakdowns: []string{"US"}},
		{DistinctID: "user-2", Times: []time.Time{t0, t1}, StepMatches: []int64{0, 1}, Breakdowns: []string{"US"}},
		// DE user: only completes step 0
		{DistinctID: "user-3", Times: []time.Time{t0}, StepMatches: []int64{0}, Breakdowns: []string{"DE"}},
		// DE user: completes both steps (2-hour gap)
		{DistinctID: "user-4", Times: []time.Time{t0, t2}, StepMatches: []int64{0, 1}, Breakdowns: []string{"DE"}},
	}

	kinds := []string{"signup", "purchase"}
	rows, err := insights.ComputeFunnelTiming(ctx, users, kinds, 0, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 2 breakdowns × 2 steps = 4 rows; insertion order: US first, then DE.
	if len(rows) != 4 {
		t.Fatalf("expected 4 rows, got %d", len(rows))
	}

	// US step 0
	if rows[0].Breakdowns[0] != "US" || rows[0].EventKind != "signup" {
		t.Errorf("row 0: got breakdown=%v kind=%v", rows[0].Breakdowns, rows[0].EventKind)
	}
	if rows[0].Value != 2 {
		t.Errorf("US step 0 count: got %v, want 2", rows[0].Value)
	}

	// US step 1: 2 users, avg = 3600s
	if rows[1].Breakdowns[0] != "US" || rows[1].EventKind != "purchase" {
		t.Errorf("row 1: got breakdown=%v kind=%v", rows[1].Breakdowns, rows[1].EventKind)
	}
	if rows[1].Value != 2 {
		t.Errorf("US step 1 count: got %v, want 2", rows[1].Value)
	}
	if rows[1].AvgConvertSeconds != 3600 {
		t.Errorf("US step 1 avg: got %v, want 3600", rows[1].AvgConvertSeconds)
	}

	// DE step 0: 2 users
	if rows[2].Breakdowns[0] != "DE" || rows[2].EventKind != "signup" {
		t.Errorf("row 2: got breakdown=%v kind=%v", rows[2].Breakdowns, rows[2].EventKind)
	}
	if rows[2].Value != 2 {
		t.Errorf("DE step 0 count: got %v, want 2", rows[2].Value)
	}

	// DE step 1: 1 user (user-4), avg = 7200s
	if rows[3].Breakdowns[0] != "DE" || rows[3].EventKind != "purchase" {
		t.Errorf("row 3: got breakdown=%v kind=%v", rows[3].Breakdowns, rows[3].EventKind)
	}
	if rows[3].Value != 1 {
		t.Errorf("DE step 1 count: got %v, want 1", rows[3].Value)
	}
	if rows[3].AvgConvertSeconds != 7200 {
		t.Errorf("DE step 1 avg: got %v, want 7200", rows[3].AvgConvertSeconds)
	}
}

func TestComputeFunnelTiming_SameKindSteps(t *testing.T) {
	// Documents the multiIf limitation: when two steps have the same kind,
	// multiIf always tags as the earlier step. The Go walk can still match
	// step 1 from a second occurrence because it looks for step_match == 1,
	// but with multiIf tagging, all events are tagged 0 — step 1 never matches.
	t0 := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(1 * time.Hour)

	users := []insights.FunnelUserEvents{
		{
			// Both events tagged as step 0 by multiIf (same kind)
			DistinctID:  "user-1",
			Times:       []time.Time{t0, t1},
			StepMatches: []int64{0, 0},
		},
	}

	rows, err := insights.ComputeFunnelTiming(ctx, users, []string{"page_view", "page_view"}, 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Step 0 matches, step 1 never matches (both tagged as 0)
	if rows[0].Value != 1 {
		t.Errorf("step 0: got %v, want 1", rows[0].Value)
	}
	if rows[1].Value != 0 {
		t.Errorf("step 1: got %v, want 0 (same-kind limitation)", rows[1].Value)
	}
}

func TestComputeFunnelTiming_WithBreakdownsAndWindow(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)

	users := []insights.FunnelUserEvents{
		// US: completes within 1-hour window
		{DistinctID: "u1", Times: []time.Time{t0, t0.Add(30 * time.Minute)}, StepMatches: []int64{0, 1}, Breakdowns: []string{"US"}},
		// DE: exceeds 1-hour window → truncated at step 0
		{DistinctID: "u2", Times: []time.Time{t0, t0.Add(2 * time.Hour)}, StepMatches: []int64{0, 1}, Breakdowns: []string{"DE"}},
	}

	rows, err := insights.ComputeFunnelTiming(ctx, users, []string{"a", "b"}, 3600, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("expected 4 rows (2 breakdowns × 2 steps), got %d", len(rows))
	}
	// US step 1: completed
	if rows[1].Value != 1 {
		t.Errorf("US step 1: got %v, want 1", rows[1].Value)
	}
	// DE step 1: window exceeded
	if rows[3].Value != 0 {
		t.Errorf("DE step 1: got %v, want 0 (window exceeded)", rows[3].Value)
	}
}

func TestComputeFunnelTiming_MultiBreakdowns(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)

	users := []insights.FunnelUserEvents{
		{DistinctID: "u1", Times: []time.Time{t0, t0.Add(time.Hour)}, StepMatches: []int64{0, 1}, Breakdowns: []string{"US", "Chrome"}},
		{DistinctID: "u2", Times: []time.Time{t0, t0.Add(time.Hour)}, StepMatches: []int64{0, 1}, Breakdowns: []string{"US", "Safari"}},
	}

	rows, err := insights.ComputeFunnelTiming(ctx, users, []string{"a", "b"}, 0, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 2 distinct breakdown combos × 2 steps = 4 rows
	if len(rows) != 4 {
		t.Fatalf("expected 4 rows, got %d", len(rows))
	}
	if rows[0].Breakdowns[0] != "US" || rows[0].Breakdowns[1] != "Chrome" {
		t.Errorf("row 0: expected [US Chrome], got %v", rows[0].Breakdowns)
	}
	if rows[2].Breakdowns[0] != "US" || rows[2].Breakdowns[1] != "Safari" {
		t.Errorf("row 2: expected [US Safari], got %v", rows[2].Breakdowns)
	}
}

// TestComputeFunnelTiming_EmptyStringBreakdownNoCollision verifies that a user with
// an empty-string breakdown value does not collide with the zero-user sentinel path.
func TestComputeFunnelTiming_EmptyStringBreakdownNoCollision(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(5 * time.Second)

	users := []insights.FunnelUserEvents{
		{
			DistinctID:  "u1",
			Times:       []time.Time{t0, t1},
			StepMatches: []int64{0, 1},
			Breakdowns:  []string{""},
		},
	}

	rows, err := insights.ComputeFunnelTiming(ctx, users, []string{"sign_up", "purchase"}, 0, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	// The user converted through both steps.
	if rows[0].Value != 1 {
		t.Errorf("step 0: expected count 1, got %v", rows[0].Value)
	}
	if rows[1].Value != 1 {
		t.Errorf("step 1: expected count 1, got %v", rows[1].Value)
	}
	if rows[0].Breakdowns[0] != "" {
		t.Errorf("step 0: expected empty-string breakdown, got %q", rows[0].Breakdowns[0])
	}
}

func TestComputeFunnelTiming_BreakdownLengthMismatch(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)

	users := []insights.FunnelUserEvents{
		{DistinctID: "u1", Times: []time.Time{t0}, StepMatches: []int64{0}, Breakdowns: []string{"US"}},
		{DistinctID: "u2", Times: []time.Time{t0}, StepMatches: []int64{0}, Breakdowns: []string{"DE", "Chrome"}},
	}

	if _, err := insights.ComputeFunnelTiming(ctx, users, []string{"a"}, 0, 1); err == nil {
		t.Fatal("expected error for mismatched breakdown lengths across users")
	}
}

func TestComputeFunnelTiming_MedianOddCount(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	// 3 users with step-1 conversion times: 60s, 120s, 180s
	users := []insights.FunnelUserEvents{
		{DistinctID: "u1", Times: []time.Time{t0, t0.Add(60 * time.Second)}, StepMatches: []int64{0, 1}},
		{DistinctID: "u2", Times: []time.Time{t0, t0.Add(120 * time.Second)}, StepMatches: []int64{0, 1}},
		{DistinctID: "u3", Times: []time.Time{t0, t0.Add(180 * time.Second)}, StepMatches: []int64{0, 1}},
	}
	rows, err := insights.ComputeFunnelTiming(ctx, users, []string{"a", "b"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	step1 := rows[1]
	if step1.MedianConvertSeconds != 120 {
		t.Errorf("median: got %v, want 120", step1.MedianConvertSeconds)
	}
	if step1.P95ConvertSeconds != 180 {
		t.Errorf("p95: got %v, want 180", step1.P95ConvertSeconds)
	}
}

func TestComputeFunnelTiming_MedianEvenCount(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	// 4 users with step-1 conversion times: 60s, 120s, 180s, 240s → median = (120+180)/2 = 150
	users := []insights.FunnelUserEvents{
		{DistinctID: "u1", Times: []time.Time{t0, t0.Add(60 * time.Second)}, StepMatches: []int64{0, 1}},
		{DistinctID: "u2", Times: []time.Time{t0, t0.Add(120 * time.Second)}, StepMatches: []int64{0, 1}},
		{DistinctID: "u3", Times: []time.Time{t0, t0.Add(180 * time.Second)}, StepMatches: []int64{0, 1}},
		{DistinctID: "u4", Times: []time.Time{t0, t0.Add(240 * time.Second)}, StepMatches: []int64{0, 1}},
	}
	rows, err := insights.ComputeFunnelTiming(ctx, users, []string{"a", "b"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	step1 := rows[1]
	if step1.MedianConvertSeconds != 150 {
		t.Errorf("median: got %v, want 150", step1.MedianConvertSeconds)
	}
	if step1.P95ConvertSeconds != 240 {
		t.Errorf("p95: got %v, want 240", step1.P95ConvertSeconds)
	}
}

func TestComputeFunnelTiming_DistributionBuckets(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	// One user per bucket: <30s, 30s-2m, 2-5m, 5-15m, 15-60m, 1-6h, 6-24h, 24h+
	deltaSecs := []float64{10, 60, 200, 600, 2000, 10000, 50000, 100000}
	users := make([]insights.FunnelUserEvents, len(deltaSecs))
	for i, d := range deltaSecs {
		users[i] = insights.FunnelUserEvents{
			DistinctID:  "u" + string(rune('0'+i)),
			Times:       []time.Time{t0, t0.Add(time.Duration(d) * time.Second)},
			StepMatches: []int64{0, 1},
		}
	}
	rows, err := insights.ComputeFunnelTiming(ctx, users, []string{"a", "b"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	dist := rows[1].ConvertSecondsDistribution
	if len(dist) != 8 {
		t.Fatalf("distribution length: got %d, want 8", len(dist))
	}
	for i, c := range dist {
		if c != 1 {
			t.Errorf("bucket %d: got %d, want 1", i, c)
		}
	}
}

func TestComputeFunnelTiming_StepZeroHasNoStats(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	users := []insights.FunnelUserEvents{
		{DistinctID: "u1", Times: []time.Time{t0, t0.Add(60 * time.Second)}, StepMatches: []int64{0, 1}},
	}
	rows, err := insights.ComputeFunnelTiming(ctx, users, []string{"a", "b"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	step0 := rows[0]
	if step0.MedianConvertSeconds != 0 {
		t.Errorf("step 0 median: got %v, want 0", step0.MedianConvertSeconds)
	}
	if step0.P95ConvertSeconds != 0 {
		t.Errorf("step 0 p95: got %v, want 0", step0.P95ConvertSeconds)
	}
	if step0.ConvertSecondsDistribution != nil {
		t.Errorf("step 0 distribution: got %v, want nil", step0.ConvertSecondsDistribution)
	}
}

func TestComputeFunnelTiming_NoConvertersStepHasZeroDistribution(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	// User only completes step 0; step 1 has count=0
	users := []insights.FunnelUserEvents{
		{DistinctID: "u1", Times: []time.Time{t0}, StepMatches: []int64{0}},
	}
	rows, err := insights.ComputeFunnelTiming(ctx, users, []string{"a", "b"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	step1 := rows[1]
	if step1.ConvertSecondsDistribution == nil {
		t.Fatal("step 1 distribution: got nil, want zero-filled slice")
	}
	if len(step1.ConvertSecondsDistribution) != 8 {
		t.Errorf("step 1 distribution length: got %d, want 8", len(step1.ConvertSecondsDistribution))
	}
	for i, c := range step1.ConvertSecondsDistribution {
		if c != 0 {
			t.Errorf("bucket %d: got %d, want 0", i, c)
		}
	}
}
