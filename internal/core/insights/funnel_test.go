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
	rows, err := insights.ComputeFunnelTiming(ctx, "", users, kinds, 0, 0)
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
	if rows[0].Timing != nil {
		t.Errorf("step 0 timing should be nil (entry step), got %+v", rows[0].Timing)
	}

	// Step 1: user-1 and user-2 (both 1 hour from step 0)
	if rows[1].Value != 2 {
		t.Errorf("step 1 count: got %v, want 2", rows[1].Value)
	}
	if rows[1].Timing.Avg != 3600*time.Second {
		t.Errorf("step 1 avg time: got %v, want 3600s", rows[1].Timing.Avg)
	}
	if rows[1].Timing.Median != 3600*time.Second {
		t.Errorf("step 1 median: got %v, want 3600s", rows[1].Timing.Median)
	}
	if rows[1].Timing.P95 != 3600*time.Second {
		t.Errorf("step 1 p95: got %v, want 3600s", rows[1].Timing.P95)
	}
	// Both deltas land in bucket 5 (1-6h, since 3600s == lower bound of "1-6h" and exclusive upper of "15-60m").
	wantStep1Dist := []int64{0, 0, 0, 0, 0, 2, 0, 0}
	for i, w := range wantStep1Dist {
		if rows[1].Timing.Distribution[i] != w {
			t.Errorf("step 1 bucket %d: got %d, want %d", i, rows[1].Timing.Distribution[i], w)
		}
	}

	// Step 2: only user-1 (2 hours from step 1)
	if rows[2].Value != 1 {
		t.Errorf("step 2 count: got %v, want 1", rows[2].Value)
	}
	if rows[2].Timing.Avg != 7200*time.Second {
		t.Errorf("step 2 avg time: got %v, want 7200s", rows[2].Timing.Avg)
	}
	if rows[2].Timing.Median != 7200*time.Second {
		t.Errorf("step 2 median: got %v, want 7200s", rows[2].Timing.Median)
	}
	if rows[2].Timing.P95 != 7200*time.Second {
		t.Errorf("step 2 p95: got %v, want 7200s", rows[2].Timing.P95)
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

	rows, err := insights.ComputeFunnelTiming(ctx, "", users, []string{"a", "b", "c"}, 3600, 0)
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
	rows, err := insights.ComputeFunnelTiming(ctx, "", users, []string{"a", "b"}, 3600, 0)
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

	rows, err := insights.ComputeFunnelTiming(ctx, "", users, []string{"a", "b"}, 0, 0)
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
	if rows[1].Timing.Avg != 3600*time.Second {
		t.Errorf("step 1 avg time: got %v, want 3600", rows[1].Timing.Avg)
	}
}

func TestComputeFunnelTiming_NoUsers(t *testing.T) {
	rows, err := insights.ComputeFunnelTiming(ctx, "", nil, []string{"a", "b"}, 0, 0)
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
	rows, err := insights.ComputeFunnelTiming(ctx, "", nil, []string{"a", "b"}, 0, 1)
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
	if _, err := insights.ComputeFunnelTiming(ctx, "", nil, nil, 0, 0); err == nil {
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

	if _, err := insights.ComputeFunnelTiming(ctx, "", users, []string{"a", "b"}, 0, 0); err == nil {
		t.Fatal("expected error for mismatched array lengths")
	}
}

// TestComputeFunnelTiming_NegativeWindowReturnsError pins the windowSec < 0 guard.
// Protovalidate enforces gte: 1s for RPC callers, but workers/scripts/tests bypass
// the interceptor — without this guard, a negative windowSec would produce a negative
// windowDur and silently disable the conversion-window check (windowDur > 0 → false).
func TestComputeFunnelTiming_NegativeWindowReturnsError(t *testing.T) {
	if _, err := insights.ComputeFunnelTiming(ctx, "", nil, []string{"a"}, -1, 0); err == nil {
		t.Fatal("expected error for negative windowSec")
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
	rows, err := insights.ComputeFunnelTiming(ctx, "", users, kinds, 0, 1)
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
	if rows[1].Timing.Avg != 3600*time.Second {
		t.Errorf("US step 1 avg: got %v, want 3600", rows[1].Timing.Avg)
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
	if rows[3].Timing.Avg != 7200*time.Second {
		t.Errorf("DE step 1 avg: got %v, want 7200", rows[3].Timing.Avg)
	}
}

func TestComputeFunnelTiming_SameKindSteps(t *testing.T) {
	// Documents the multiIf limitation: when two steps have the same kind,
	// multiIf short-circuits on the first matching arm and tags every event
	// as the earlier step. If multiIf could tag the second occurrence as
	// step 1, the Go walk's `step_match == matched` check would advance the
	// funnel — but multiIf never produces a step-1 tag here, so step 1
	// never matches.
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

	rows, err := insights.ComputeFunnelTiming(ctx, "", users, []string{"page_view", "page_view"}, 0, 0)
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
		// DE: step 1 exceeds the 1-hour window → user counted at step 0 only
		{DistinctID: "u2", Times: []time.Time{t0, t0.Add(2 * time.Hour)}, StepMatches: []int64{0, 1}, Breakdowns: []string{"DE"}},
	}

	rows, err := insights.ComputeFunnelTiming(ctx, "", users, []string{"a", "b"}, 3600, 1)
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

	rows, err := insights.ComputeFunnelTiming(ctx, "", users, []string{"a", "b"}, 0, 2)
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

	rows, err := insights.ComputeFunnelTiming(ctx, "", users, []string{"sign_up", "purchase"}, 0, 1)
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

	if _, err := insights.ComputeFunnelTiming(ctx, "", users, []string{"a"}, 0, 1); err == nil {
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
	rows, err := insights.ComputeFunnelTiming(ctx, "", users, []string{"a", "b"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	step1 := rows[1]
	if step1.Timing.Median != 120*time.Second {
		t.Errorf("median: got %v, want 120s", step1.Timing.Median)
	}
	if step1.Timing.P95 != 180*time.Second {
		t.Errorf("p95: got %v, want 180s", step1.Timing.P95)
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
	rows, err := insights.ComputeFunnelTiming(ctx, "", users, []string{"a", "b"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	step1 := rows[1]
	if step1.Timing.Median != 150*time.Second {
		t.Errorf("median: got %v, want 150s", step1.Timing.Median)
	}
	if step1.Timing.P95 != 240*time.Second {
		t.Errorf("p95: got %v, want 240s", step1.Timing.P95)
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
	rows, err := insights.ComputeFunnelTiming(ctx, "", users, []string{"a", "b"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	dist := rows[1].Timing.Distribution
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
	rows, err := insights.ComputeFunnelTiming(ctx, "", users, []string{"a", "b"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	step0 := rows[0]
	if step0.Timing != nil {
		t.Errorf("step 0: Timing should be nil (entry step), got %+v", step0.Timing)
	}
}

func TestComputeFunnelTiming_NoConvertersStepHasZeroDistribution(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	// User only completes step 0; step 1 has count=0
	users := []insights.FunnelUserEvents{
		{DistinctID: "u1", Times: []time.Time{t0}, StepMatches: []int64{0}},
	}
	rows, err := insights.ComputeFunnelTiming(ctx, "", users, []string{"a", "b"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	step1 := rows[1]
	if step1.Timing == nil {
		t.Fatal("step 1: Timing should be non-nil even with zero converters")
	}
	if step1.Timing.Distribution == nil {
		t.Fatal("step 1 distribution: got nil, want zero-filled slice")
	}
	if len(step1.Timing.Distribution) != 8 {
		t.Errorf("step 1 distribution length: got %d, want 8", len(step1.Timing.Distribution))
	}
	for i, c := range step1.Timing.Distribution {
		if c != 0 {
			t.Errorf("bucket %d: got %d, want 0", i, c)
		}
	}
	if step1.Timing.Median != 0 {
		t.Errorf("zero-converter step median: got %v, want 0", step1.Timing.Median)
	}
	if step1.Timing.P95 != 0 {
		t.Errorf("zero-converter step p95: got %v, want 0", step1.Timing.P95)
	}
	if step1.Timing.Avg != 0 {
		t.Errorf("zero-converter step avg: got %v, want 0", step1.Timing.Avg)
	}
}

// TestComputeFunnelTiming_MidFunnelZeroConvertersHasZeroDistribution verifies that an
// intermediate (non-final) step with zero converters produces a non-nil Timing with an
// 8-element zero-filled Distribution — same contract as the last step. Guards against
// step-position-dependent regressions that might handle the final step specially.
func TestComputeFunnelTiming_MidFunnelZeroConvertersHasZeroDistribution(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	// 3-step funnel; user only completes step 0, so step 1 (mid-funnel) and step 2 (last)
	// both have zero converters by greedy semantics.
	users := []insights.FunnelUserEvents{
		{DistinctID: "u1", Times: []time.Time{t0}, StepMatches: []int64{0}},
	}
	rows, err := insights.ComputeFunnelTiming(ctx, "", users, []string{"a", "b", "c"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}
	for _, idx := range []int{1, 2} {
		row := rows[idx]
		if row.Timing == nil {
			t.Fatalf("step %d: Timing should be non-nil with zero-filled Distribution", idx)
		}
		if len(row.Timing.Distribution) != 8 {
			t.Errorf("step %d distribution length: got %d, want 8", idx, len(row.Timing.Distribution))
		}
		for i, c := range row.Timing.Distribution {
			if c != 0 {
				t.Errorf("step %d bucket %d: got %d, want 0", idx, i, c)
			}
		}
	}
}

// TestComputeFunnelTiming_WindowTruncationPerBreakdownIsolatesTiming verifies that when
// the conversion window truncates some users in each breakdown group, the per-series
// timing stats reflect only that group's non-truncated users — no cross-series leak.
func TestComputeFunnelTiming_WindowTruncationPerBreakdownIsolatesTiming(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	windowSec := int64(60) // 1-minute window

	users := []insights.FunnelUserEvents{
		// US: u1 within window (10s delta), u2 truncated (120s delta)
		{DistinctID: "us-1", Times: []time.Time{t0, t0.Add(10 * time.Second)}, StepMatches: []int64{0, 1}, Breakdowns: []string{"US"}},
		{DistinctID: "us-2", Times: []time.Time{t0, t0.Add(120 * time.Second)}, StepMatches: []int64{0, 1}, Breakdowns: []string{"US"}},
		// DE: d1 within window (50s delta), d2 truncated (180s delta)
		{DistinctID: "de-1", Times: []time.Time{t0, t0.Add(50 * time.Second)}, StepMatches: []int64{0, 1}, Breakdowns: []string{"DE"}},
		{DistinctID: "de-2", Times: []time.Time{t0, t0.Add(180 * time.Second)}, StepMatches: []int64{0, 1}, Breakdowns: []string{"DE"}},
	}

	rows, err := insights.ComputeFunnelTiming(ctx, "", users, []string{"a", "b"}, windowSec, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("expected 4 rows (2 breakdowns × 2 steps), got %d", len(rows))
	}

	var usStep1, deStep1 insights.FunnelRow
	for _, r := range rows {
		if r.StepIndex != 1 {
			continue
		}
		switch r.Breakdowns[0] {
		case "US":
			usStep1 = r
		case "DE":
			deStep1 = r
		}
	}

	// US: only u1 (10s) contributes to step 1; u2 truncated.
	if usStep1.Value != 1 {
		t.Errorf("US step 1 count: got %v, want 1 (u2 truncated)", usStep1.Value)
	}
	if usStep1.Timing.Avg != 10*time.Second {
		t.Errorf("US step 1 avg: got %v, want 10s (u2 must not contribute)", usStep1.Timing.Avg)
	}
	if usStep1.Timing.Median != 10*time.Second {
		t.Errorf("US step 1 median: got %v, want 10s", usStep1.Timing.Median)
	}

	// DE: only d1 (50s) contributes to step 1; d2 truncated.
	if deStep1.Value != 1 {
		t.Errorf("DE step 1 count: got %v, want 1 (d2 truncated)", deStep1.Value)
	}
	if deStep1.Timing.Avg != 50*time.Second {
		t.Errorf("DE step 1 avg: got %v, want 50s (d2 must not contribute)", deStep1.Timing.Avg)
	}
	if deStep1.Timing.Median != 50*time.Second {
		t.Errorf("DE step 1 median: got %v, want 50s", deStep1.Timing.Median)
	}

	// Sanity: stats must differ — equality would suggest cross-series pollution.
	if usStep1.Timing.Avg == deStep1.Timing.Avg {
		t.Errorf("US and DE avg must differ; both = %v (cross-series leak?)", usStep1.Timing.Avg)
	}
}

// TestComputeFunnelTiming_PerBreakdownTimingConvergence verifies that median, p95, and
// the distribution histogram are computed independently per breakdown series — i.e. that
// stats do not pool across breakdown keys. Without per-series aggregation, the US median
// (which uses sub-second deltas) would be polluted by the DE deltas (multi-hour deltas)
// or vice versa, and the regression would not show in the count-or-avg-only assertions
// that earlier tests already check.
func TestComputeFunnelTiming_PerBreakdownTimingConvergence(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)

	users := []insights.FunnelUserEvents{
		// US: deltas 10s and 20s → median 15, p95 20, both in bucket 0 ("0-30s")
		{DistinctID: "us-1", Times: []time.Time{t0, t0.Add(10 * time.Second)}, StepMatches: []int64{0, 1}, Breakdowns: []string{"US"}},
		{DistinctID: "us-2", Times: []time.Time{t0, t0.Add(20 * time.Second)}, StepMatches: []int64{0, 1}, Breakdowns: []string{"US"}},

		// DE: deltas 600s, 1200s, 7200s → median 1200, p95 7200, distribution spans buckets 2/4/6
		{DistinctID: "de-1", Times: []time.Time{t0, t0.Add(600 * time.Second)}, StepMatches: []int64{0, 1}, Breakdowns: []string{"DE"}},
		{DistinctID: "de-2", Times: []time.Time{t0, t0.Add(1200 * time.Second)}, StepMatches: []int64{0, 1}, Breakdowns: []string{"DE"}},
		{DistinctID: "de-3", Times: []time.Time{t0, t0.Add(7200 * time.Second)}, StepMatches: []int64{0, 1}, Breakdowns: []string{"DE"}},
	}

	rows, err := insights.ComputeFunnelTiming(ctx, "", users, []string{"signup", "purchase"}, 0, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 2 breakdowns × 2 steps = 4 rows; insertion order: US first (first user seen), then DE.
	if len(rows) != 4 {
		t.Fatalf("expected 4 rows, got %d", len(rows))
	}

	usStep1 := rows[1]
	deStep1 := rows[3]

	if usStep1.Breakdowns[0] != "US" || deStep1.Breakdowns[0] != "DE" {
		t.Fatalf("breakdown ordering: got rows[1]=%v rows[3]=%v", usStep1.Breakdowns, deStep1.Breakdowns)
	}

	// US: median = (10+20)/2 = 15
	if usStep1.Timing.Median != 15*time.Second {
		t.Errorf("US median: got %v, want 15s", usStep1.Timing.Median)
	}
	// US: ceil(0.95*2)=2 → idx 1 → 20
	if usStep1.Timing.P95 != 20*time.Second {
		t.Errorf("US p95: got %v, want 20s", usStep1.Timing.P95)
	}
	// US distribution: both in bucket 0, all others zero
	wantUS := []int64{2, 0, 0, 0, 0, 0, 0, 0}
	for i, w := range wantUS {
		if usStep1.Timing.Distribution[i] != w {
			t.Errorf("US bucket %d: got %d, want %d", i, usStep1.Timing.Distribution[i], w)
		}
	}

	// DE: 3 deltas — odd length, median is middle = 1200
	if deStep1.Timing.Median != 1200*time.Second {
		t.Errorf("DE median: got %v, want 1200s", deStep1.Timing.Median)
	}
	// DE: ceil(0.95*3)=3 → idx 2 → 7200
	if deStep1.Timing.P95 != 7200*time.Second {
		t.Errorf("DE p95: got %v, want 7200s", deStep1.Timing.P95)
	}
	// DE distribution: 600s in bucket 3 (<900), 1200s in bucket 4 (<3600), 7200s in bucket 5 (<21600).
	wantDE := []int64{0, 0, 0, 1, 1, 1, 0, 0}
	for i, w := range wantDE {
		if deStep1.Timing.Distribution[i] != w {
			t.Errorf("DE bucket %d: got %d, want %d", i, deStep1.Timing.Distribution[i], w)
		}
	}

	// Sanity: the medians MUST differ — that's the regression a pooled aggregation would hide.
	if usStep1.Timing.Median == deStep1.Timing.Median {
		t.Errorf("series medians should differ; got both = %v (stats pooled across breakdowns?)", usStep1.Timing.Median)
	}
}

// TestComputeFunnelTiming_WindowTruncationExcludesFromTiming verifies that a user truncated
// at step N by the conversion window does not contribute to step N's median, p95, or
// distribution — only to step N-1's. Without this, the timing fields would silently include
// or exclude users inconsistently with the count.
func TestComputeFunnelTiming_WindowTruncationExcludesFromTiming(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	windowSec := int64(3600) // 1 hour

	users := []insights.FunnelUserEvents{
		// All three reach step 1 at t0+30s (delta 30s from step 0).
		// Steps 0, 1, 2:
		//   user-A: step 2 at t0+60s   (delta 30s, within window) → contributes
		//   user-B: step 2 at t0+3700s (delta 3670s, EXCEEDS window) → truncated, excluded from step 2
		//   user-C: step 2 at t0+90s   (delta 60s, within window) → contributes
		{DistinctID: "user-A", Times: []time.Time{t0, t0.Add(30 * time.Second), t0.Add(60 * time.Second)}, StepMatches: []int64{0, 1, 2}},
		{DistinctID: "user-B", Times: []time.Time{t0, t0.Add(30 * time.Second), t0.Add(3700 * time.Second)}, StepMatches: []int64{0, 1, 2}},
		{DistinctID: "user-C", Times: []time.Time{t0, t0.Add(30 * time.Second), t0.Add(90 * time.Second)}, StepMatches: []int64{0, 1, 2}},
	}

	rows, err := insights.ComputeFunnelTiming(ctx, "", users, []string{"a", "b", "c"}, windowSec, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Step 1: all three users contribute (delta = 30s each).
	if rows[1].Value != 3 {
		t.Errorf("step 1 count: got %v, want 3", rows[1].Value)
	}

	// Step 2: only A (30s) and C (60s); B was truncated.
	if rows[2].Value != 2 {
		t.Errorf("step 2 count: got %v, want 2 (B truncated)", rows[2].Value)
	}
	// Median of [30, 60] = 45
	if rows[2].Timing.Median != 45*time.Second {
		t.Errorf("step 2 median: got %v, want 45s (truncated B must not contribute)", rows[2].Timing.Median)
	}
	// p95 of [30, 60]: ceil(0.95*2)=2 → idx 1 → 60
	if rows[2].Timing.P95 != 60*time.Second {
		t.Errorf("step 2 p95: got %v, want 60s", rows[2].Timing.P95)
	}
	// Distribution: both in bucket 1 (30-120s); B's 3670s would land in bucket 4 (1-6h) if leaked.
	if rows[2].Timing.Distribution[1] != 2 {
		t.Errorf("step 2 bucket 1 (30-120s): got %d, want 2", rows[2].Timing.Distribution[1])
	}
	if rows[2].Timing.Distribution[4] != 0 {
		t.Errorf("step 2 bucket 4 (1-6h): got %d, want 0 (truncated user must not leak into distribution)", rows[2].Timing.Distribution[4])
	}
}
