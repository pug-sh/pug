package insights

import (
	"math"
	"time"
)

// funnelBucket is one entry of a histogram-bucket table. openEnded marks the final bucket;
// its upper field is a sentinel and is not consulted for classification.
type funnelBucket struct {
	upper     time.Duration
	label     string
	openEnded bool
}

// funnelTimingBuckets defines the 8 fixed buckets for funnel conversion-time histograms.
// Each upper bound is exclusive. Last bucket is open-ended; upper is a math.MaxInt64
// sentinel — the openEnded flag short-circuits classification first, but the sentinel
// also keeps "v >= upper" false for any plausible finite duration, so a future caller
// that forgets the flag still terminates correctly.
var funnelTimingBuckets = []funnelBucket{
	{30 * time.Second, "0-30s", false},
	{2 * time.Minute, "30s-2m", false},
	{5 * time.Minute, "2-5m", false},
	{15 * time.Minute, "5-15m", false},
	{1 * time.Hour, "15-60m", false},
	{6 * time.Hour, "1-6h", false},
	{24 * time.Hour, "6-24h", false},
	{time.Duration(math.MaxInt64), "24h+", true},
}

// newStepTiming returns a *StepTiming with a zero-filled Distribution of canonical
// length len(funnelTimingBuckets). Centralising allocation here makes the
// "Distribution length matches the bucket table" invariant structural, so the
// proto-translation layer can index bucket metadata in lock-step without bounds checks.
func newStepTiming() *StepTiming {
	return &StepTiming{Distribution: make([]int64, len(funnelTimingBuckets))}
}

// medianSorted returns the median of a pre-sorted slice using average-of-two-middles for even length.
// The bool is false when sorted is empty, distinguishing "no data" from a real zero median.
func medianSorted(sorted []time.Duration) (time.Duration, bool) {
	n := len(sorted)
	if n == 0 {
		return 0, false
	}
	if n%2 == 1 {
		return sorted[n/2], true
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2, true
}

// percentileSorted returns the p-th percentile of a pre-sorted slice using nearest-rank ceiling.
// p must be in (0, 1]; NaN or out-of-range p yields (0, false), as does an empty slice.
// The bool distinguishes valid results from "no data" or invalid input.
func percentileSorted(sorted []time.Duration, p float64) (time.Duration, bool) {
	n := len(sorted)
	if n == 0 || math.IsNaN(p) || p <= 0 || p > 1 {
		return 0, false
	}
	idx := max(int(math.Ceil(p*float64(n)))-1, 0)
	if idx >= n {
		idx = n - 1
	}
	return sorted[idx], true
}

// distributionCountsSorted buckets a pre-sorted slice into the package-level
// funnelTimingBuckets table (exclusive upper bounds). Single-pass: walks the input once
// while advancing a non-decreasing bucket index. Empty input returns a zero-filled slice
// of length len(funnelTimingBuckets), preserving "no converters" as all-zero counts.
// The inner advance halts on the open-ended bucket, which catches everything beyond the
// last finite bound.
func distributionCountsSorted(sorted []time.Duration) []int64 {
	counts := make([]int64, len(funnelTimingBuckets))
	b := 0
	for _, v := range sorted {
		for !funnelTimingBuckets[b].openEnded && v >= funnelTimingBuckets[b].upper {
			b++
		}
		counts[b]++
	}
	return counts
}
