package insights

import (
	"math"
	"testing"
)

func TestMedianFloat(t *testing.T) {
	tests := []struct {
		name   string
		input  []float64
		expect float64
	}{
		{"empty", nil, 0},
		{"single", []float64{42}, 42},
		{"odd length", []float64{10, 20, 30}, 20},
		{"even length", []float64{10, 20, 30, 40}, 25},
		{"even length two middles", []float64{1, 3}, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := medianFloat(tc.input)
			if got != tc.expect {
				t.Errorf("got %v, want %v", got, tc.expect)
			}
		})
	}
}

func TestPercentileFloat(t *testing.T) {
	tests := []struct {
		name   string
		input  []float64
		p      float64
		expect float64
	}{
		{"empty", nil, 0.95, 0},
		{"single p95", []float64{100}, 0.95, 100},
		{"three p95 ceiling", []float64{10, 20, 30}, 0.95, 30},         // ceil(0.95*3)=3 → idx 2
		{"four p95", []float64{10, 20, 30, 40}, 0.95, 40},              // ceil(0.95*4)=4 → idx 3
		{"p50 odd", []float64{10, 20, 30}, 0.50, 20},                   // ceil(0.5*3)=2 → idx 1
		{"twenty p95", []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}, 0.95, 19}, // ceil(0.95*20)=19 → idx 18
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := percentileFloat(tc.input, tc.p)
			if got != tc.expect {
				t.Errorf("got %v, want %v", got, tc.expect)
			}
		})
	}
}

func TestDistributionCounts(t *testing.T) {
	bounds := funnelTimingBucketUpperSec

	t.Run("empty input", func(t *testing.T) {
		counts := distributionCounts(nil, bounds)
		for i, c := range counts {
			if c != 0 {
				t.Errorf("bucket %d: got %d, want 0", i, c)
			}
		}
	})

	t.Run("one per bucket", func(t *testing.T) {
		// Values chosen to land in each of the 8 buckets.
		// Buckets: <30, <120, <300, <900, <3600, <21600, <86400, >=86400
		input := []float64{10, 60, 200, 600, 2000, 10000, 50000, 100000}
		counts := distributionCounts(input, bounds)
		for i, c := range counts {
			if c != 1 {
				t.Errorf("bucket %d: got %d, want 1", i, c)
			}
		}
	})

	t.Run("value at exact boundary goes to next bucket", func(t *testing.T) {
		// 30 is exactly funnelTimingBucketUpperSec[0]; should land in bucket 1 ("30s-2m")
		counts := distributionCounts([]float64{30}, bounds)
		if counts[0] != 0 {
			t.Errorf("bucket 0: got %d, want 0", counts[0])
		}
		if counts[1] != 1 {
			t.Errorf("bucket 1: got %d, want 1", counts[1])
		}
	})

	t.Run("last bucket catches MaxFloat64 sentinel", func(t *testing.T) {
		counts := distributionCounts([]float64{math.MaxFloat64}, bounds)
		last := len(counts) - 1
		if counts[last] != 1 {
			t.Errorf("last bucket: got %d, want 1", counts[last])
		}
	})
}
