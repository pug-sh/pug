package insights

import "math"

// funnelTimingBucketUpperSec[i] is the exclusive upper bound in seconds for bucket i.
// The last entry is math.MaxFloat64 (open-ended "24h+" bucket).
var funnelTimingBucketUpperSec = []float64{30, 120, 300, 900, 3600, 21600, 86400, math.MaxFloat64}

var funnelTimingBucketLabels = []string{"0-30s", "30s-2m", "2-5m", "5-15m", "15-60m", "1-6h", "6-24h", "24h+"}

// medianFloat returns the median of a pre-sorted slice.
// Uses average-of-two-middles for even-length slices.
// Returns 0 for an empty slice.
func medianFloat(sorted []float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// percentileFloat returns the p-th percentile (0 < p <= 1) of a pre-sorted slice
// using the nearest-rank ceiling method. Returns 0 for an empty slice.
func percentileFloat(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	idx := int(math.Ceil(p*float64(n))) - 1
	if idx < 0 {
		idx = 0
	}
	return sorted[idx]
}

// distributionCounts counts how many values in the pre-sorted slice fall into each bucket
// defined by upperBounds (exclusive upper bound per bucket). O(n) two-pointer scan.
func distributionCounts(sorted []float64, upperBounds []float64) []int64 {
	counts := make([]int64, len(upperBounds))
	b := 0
	for _, v := range sorted {
		for b < len(upperBounds)-1 && v >= upperBounds[b] {
			b++
		}
		counts[b]++
	}
	return counts
}
