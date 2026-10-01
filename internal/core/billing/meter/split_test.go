package meter

import (
	"slices"
	"testing"
)

func TestSplit(t *testing.T) {
	bounds := []int64{2_000_000, 15_000_000}
	cases := []struct {
		name      string
		total     int64
		allowance int64
		tierUpTo  []int64
		want      []int64
	}{
		{"nothing sent", 0, 100_000, bounds, []int64{0, 0, 0}},
		{"exactly the allowance", 100_000, 100_000, bounds, []int64{0, 0, 0}},
		{"one past the allowance", 100_001, 100_000, bounds, []int64{1, 0, 0}},
		{"exactly the first bound", 2_000_000, 100_000, bounds, []int64{1_900_000, 0, 0}},
		{"one past the first bound", 2_000_001, 100_000, bounds, []int64{1_900_000, 1, 0}},
		{"exactly the second bound", 15_000_000, 100_000, bounds, []int64{1_900_000, 13_000_000, 0}},
		{"into the unbounded tier", 20_000_000, 100_000, bounds, []int64{1_900_000, 13_000_000, 5_000_000}},
		{"a deal allowing fewer than the default", 3_000_000, 1, bounds, []int64{1_999_999, 1_000_000, 0}},
		{"a deal allowing past the first bound", 20_000_000, 5_000_000, bounds, []int64{0, 10_000_000, 5_000_000}},
		{"no allowance at all", 10, 0, bounds, []int64{10, 0, 0}},
		{"a plan of one unbounded tier", 1_000, 100, nil, []int64{900}},
		// Neither occurs — a count is never negative, and the catalog and the ledger
		// both refuse a negative allowance — but neither may produce a negative tier.
		{"a negative total", -5, 100_000, bounds, []int64{0, 0, 0}},
		{"a negative allowance counts as none", 10, -5, bounds, []int64{10, 0, 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Split(tc.total, tc.allowance, tc.tierUpTo)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Split(%d, %d, %v) = %v, want %v", tc.total, tc.allowance, tc.tierUpTo, got, tc.want)
			}
		})
	}
}

// The tiers must always sum to what is billable, or an event is billed twice or
// not at all.
func TestSplitSumsToWhatIsBillable(t *testing.T) {
	bounds := []int64{2_000_000, 15_000_000, 50_000_000}
	for _, allowance := range []int64{0, 1, 100_000, 2_000_000, 60_000_000} {
		for _, total := range []int64{0, 1, 99_999, 100_000, 1_999_999, 2_000_000, 14_999_999, 15_000_001, 50_000_000, 70_000_000} {
			var sum int64
			for _, n := range Split(total, allowance, bounds) {
				if n < 0 {
					t.Fatalf("Split(%d, %d) has a negative tier", total, allowance)
				}
				sum += n
			}
			if want := max(0, total-allowance); sum != want {
				t.Errorf("Split(%d, %d) sums to %d, want %d", total, allowance, sum, want)
			}
		}
	}
}
