package insights_test

import (
	"testing"
	"time"

	"github.com/fivebitsio/cotton/internal/core/insights"
)

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
	rows, err := insights.ComputeFunnelTiming(users, kinds, 0)
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

	rows, err := insights.ComputeFunnelTiming(users, []string{"a", "b", "c"}, 3600)
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
	rows, err := insights.ComputeFunnelTiming(users, []string{"a", "b"}, 3600)
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

	rows, err := insights.ComputeFunnelTiming(users, []string{"a", "b"}, 0)
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
	rows, err := insights.ComputeFunnelTiming(nil, []string{"a", "b"}, 0)
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

func TestComputeFunnelTiming_EmptyKindsReturnsError(t *testing.T) {
	if _, err := insights.ComputeFunnelTiming(nil, nil, 0); err == nil {
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

	if _, err := insights.ComputeFunnelTiming(users, []string{"a", "b"}, 0); err == nil {
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
	rows, err := insights.ComputeFunnelTiming(users, kinds, 0)
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

	rows, err := insights.ComputeFunnelTiming(users, []string{"page_view", "page_view"}, 0)
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
