package insights

import (
	"math"
	"testing"
	"time"
)

func TestMedianSorted(t *testing.T) {
	tests := []struct {
		name     string
		input    []time.Duration
		expect   time.Duration
		expectOK bool
	}{
		{"empty", nil, 0, false},
		{"single", []time.Duration{42 * time.Second}, 42 * time.Second, true},
		{"odd length", []time.Duration{10 * time.Second, 20 * time.Second, 30 * time.Second}, 20 * time.Second, true},
		{"even length integer average", []time.Duration{10 * time.Second, 20 * time.Second, 30 * time.Second, 40 * time.Second}, 25 * time.Second, true},
		{"even length two middles", []time.Duration{1 * time.Second, 3 * time.Second}, 2 * time.Second, true},
		{"even length sub-second average", []time.Duration{1 * time.Second, 2 * time.Second}, 1500 * time.Millisecond, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := medianSorted(tc.input)
			if got != tc.expect || ok != tc.expectOK {
				t.Errorf("got (%v, %v), want (%v, %v)", got, ok, tc.expect, tc.expectOK)
			}
		})
	}
}

func TestPercentileSorted(t *testing.T) {
	d := func(s int) time.Duration { return time.Duration(s) * time.Second }
	tests := []struct {
		name     string
		input    []time.Duration
		p        float64
		expect   time.Duration
		expectOK bool
	}{
		{"empty", nil, 0.95, 0, false},
		{"single p95", []time.Duration{d(100)}, 0.95, d(100), true},
		{"three p95 ceiling", []time.Duration{d(10), d(20), d(30)}, 0.95, d(30), true}, // ceil(0.95*3)=3 → idx 2
		{"four p95", []time.Duration{d(10), d(20), d(30), d(40)}, 0.95, d(40), true},   // ceil(0.95*4)=4 → idx 3
		{"p50 odd", []time.Duration{d(10), d(20), d(30)}, 0.50, d(20), true},           // ceil(0.5*3)=2 → idx 1
		{"twenty p95", []time.Duration{d(1), d(2), d(3), d(4), d(5), d(6), d(7), d(8), d(9), d(10), d(11), d(12), d(13), d(14), d(15), d(16), d(17), d(18), d(19), d(20)}, 0.95, d(19), true}, // ceil(0.95*20)=19 → idx 18
		{"p=1.0 returns last", []time.Duration{d(10), d(20), d(30), d(40)}, 1.0, d(40), true},                                                                                                 // ceil(4)=4 → idx 3
		{"p=0.0 rejected", []time.Duration{d(10), d(20), d(30)}, 0.0, 0, false},                                                                                                               // p must be in (0, 1]
		{"p>1 rejected", []time.Duration{d(10), d(20), d(30)}, 1.5, 0, false},
		{"p<0 rejected", []time.Duration{d(10), d(20), d(30)}, -0.1, 0, false},
		{"p=NaN rejected", []time.Duration{d(10), d(20), d(30)}, math.NaN(), 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := percentileSorted(tc.input, tc.p)
			if got != tc.expect || ok != tc.expectOK {
				t.Errorf("got (%v, %v), want (%v, %v)", got, ok, tc.expect, tc.expectOK)
			}
		})
	}
}

func TestDistributionCountsSorted(t *testing.T) {
	d := func(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

	t.Run("empty input", func(t *testing.T) {
		counts := distributionCountsSorted(nil)
		if len(counts) != len(funnelTimingBuckets) {
			t.Fatalf("expected %d-length slice for empty input, got %d", len(funnelTimingBuckets), len(counts))
		}
		for i, c := range counts {
			if c != 0 {
				t.Errorf("bucket %d: got %d, want 0", i, c)
			}
		}
	})

	t.Run("one per bucket", func(t *testing.T) {
		// Values chosen to land in each of the 8 buckets.
		// Buckets: <30s, <2m, <5m, <15m, <1h, <6h, <24h, >=24h
		input := []time.Duration{d(10), d(60), d(200), d(600), d(2000), d(10000), d(50000), d(100000)}
		counts := distributionCountsSorted(input)
		for i, c := range counts {
			if c != 1 {
				t.Errorf("bucket %d: got %d, want 1", i, c)
			}
		}
	})

	t.Run("exact boundary promotes to next bucket", func(t *testing.T) {
		// Each boundary is exclusive: a value equal to the upper bound belongs to the NEXT bucket.
		cases := []struct {
			name     string
			value    time.Duration
			expected int // expected bucket index
		}{
			{"30s boundary", 30 * time.Second, 1},
			{"120s boundary", 2 * time.Minute, 2},
			{"300s boundary", 5 * time.Minute, 3},
			{"900s boundary", 15 * time.Minute, 4},
			{"3600s boundary", 1 * time.Hour, 5},
			{"21600s boundary", 6 * time.Hour, 6},
			{"86400s boundary", 24 * time.Hour, 7},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				counts := distributionCountsSorted([]time.Duration{c.value})
				for i, got := range counts {
					want := int64(0)
					if i == c.expected {
						want = 1
					}
					if got != want {
						t.Errorf("bucket %d: got %d, want %d", i, got, want)
					}
				}
			})
		}
	})

	t.Run("sub-second precision near boundary", func(t *testing.T) {
		// 29.9999999s lands in bucket 0 (< 30s); 30.0s and 30.0000001s land in bucket 1.
		below := distributionCountsSorted([]time.Duration{30*time.Second - time.Nanosecond})
		if below[0] != 1 {
			t.Errorf("just-below 30s: expected bucket 0=1, got %v", below)
		}
		justAt := distributionCountsSorted([]time.Duration{30 * time.Second})
		if justAt[1] != 1 {
			t.Errorf("exact 30s: expected bucket 1=1, got %v", justAt)
		}
		justAbove := distributionCountsSorted([]time.Duration{30*time.Second + time.Nanosecond})
		if justAbove[1] != 1 {
			t.Errorf("just-above 30s: expected bucket 1=1, got %v", justAbove)
		}
	})

	t.Run("varied multi-value distribution", func(t *testing.T) {
		// 3 in bucket 0, 1 in bucket 2, 0 in bucket 3, 2 in bucket 5, 0 elsewhere.
		input := []time.Duration{d(10), d(15), d(20), d(200), d(5000), d(6000)}
		counts := distributionCountsSorted(input)
		want := []int64{3, 0, 1, 0, 0, 2, 0, 0}
		for i, w := range want {
			if counts[i] != w {
				t.Errorf("bucket %d: got %d, want %d", i, counts[i], w)
			}
		}
	})

	t.Run("last bucket catches very large values", func(t *testing.T) {
		counts := distributionCountsSorted([]time.Duration{time.Duration(math.MaxInt64) - 1})
		last := len(counts) - 1
		if counts[last] != 1 {
			t.Errorf("last bucket: got %d, want 1", counts[last])
		}
	})
}

// TestFunnelTimingBuckets_Invariants locks in the structural invariants on the package-level
// bucket slice: strictly ascending finite bounds, exactly one open-ended bucket positioned last,
// MaxInt64 sentinel on the open-ended bucket, and the canonical label strings clients display.
func TestFunnelTimingBuckets_Invariants(t *testing.T) {
	if len(funnelTimingBuckets) != 8 {
		t.Fatalf("expected 8 buckets, got %d", len(funnelTimingBuckets))
	}
	openEndedCount := 0
	for _, b := range funnelTimingBuckets {
		if b.openEnded {
			openEndedCount++
		}
	}
	if openEndedCount != 1 {
		t.Errorf("expected exactly one open-ended bucket, got %d", openEndedCount)
	}
	for i, b := range funnelTimingBuckets[:len(funnelTimingBuckets)-1] {
		if b.openEnded {
			t.Errorf("bucket %d (not last): openEnded should be false", i)
		}
		if i > 0 && b.upper <= funnelTimingBuckets[i-1].upper {
			t.Errorf("buckets not strictly ascending at index %d (%v <= %v)",
				i, b.upper, funnelTimingBuckets[i-1].upper)
		}
	}
	last := funnelTimingBuckets[len(funnelTimingBuckets)-1]
	if !last.openEnded {
		t.Errorf("last bucket should be openEnded, got openEnded=false")
	}
	if last.upper != time.Duration(math.MaxInt64) {
		t.Errorf("last bucket upper should be time.Duration(math.MaxInt64), got %v", last.upper)
	}

	// Pin the exact label strings — clients display these as-is, so a typo would silently
	// propagate to every consumer. Update both this list and CLAUDE.md if labels change.
	wantLabels := []string{"0-30s", "30s-2m", "2-5m", "5-15m", "15-60m", "1-6h", "6-24h", "24h+"}
	for i, want := range wantLabels {
		if got := funnelTimingBuckets[i].label; got != want {
			t.Errorf("bucket %d label: got %q, want %q", i, got, want)
		}
	}
}
