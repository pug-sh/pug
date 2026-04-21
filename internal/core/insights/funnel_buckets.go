package insights

import "math"

// funnelTimingBucketUpperSec[i] is the exclusive upper bound in seconds for bucket i; last entry is MaxFloat64 ("24h+").
var funnelTimingBucketUpperSec = []float64{30, 120, 300, 900, 3600, 21600, 86400, math.MaxFloat64}

var funnelTimingBucketLabels = []string{"0-30s", "30s-2m", "2-5m", "5-15m", "15-60m", "1-6h", "6-24h", "24h+"}

// medianFloat returns the median of a pre-sorted slice (average-of-two-middles for even length; 0 for empty).
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

// percentileFloat returns the p-th percentile of a pre-sorted slice using nearest-rank ceiling; 0 for empty.
func percentileFloat(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	idx := max(int(math.Ceil(p*float64(n)))-1, 0)
	return sorted[idx]
}

// distributionCounts buckets a pre-sorted slice into upperBounds-defined ranges (exclusive); O(n) two-pointer.
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
