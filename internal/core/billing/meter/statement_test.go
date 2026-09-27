package meter

import (
	"slices"
	"testing"
	"time"
)

func TestCarry(t *testing.T) {
	// The previous window finished at 1.95M in tier 1 but only 1.9M was stated.
	got := carry([]int64{1_950_000, 0, 0}, []int64{1_900_000, 0, 0})
	if want := []int64{50_000, 0, 0}; !slices.Equal(got, want) {
		t.Fatalf("carry = %v, want %v", got, want)
	}
	// Stated past the final count — an erasure recount — carries nothing, never a
	// negative that would lower the next period.
	if got := carry([]int64{10, 0}, []int64{20, 0}); !slices.Equal(got, []int64{0, 0}) {
		t.Fatalf("carry below what was stated = %v, want zeros", got)
	}
}

func TestVectorHelpers(t *testing.T) {
	if got := maxEach([]int64{1, 5, 0}, []int64{3, 2, 0}); !slices.Equal(got, []int64{3, 5, 0}) {
		t.Errorf("maxEach = %v", got)
	}
	if got := sumEach([]int64{1, 5}, []int64{3, 2}); !slices.Equal(got, []int64{4, 7}) {
		t.Errorf("sumEach = %v", got)
	}
}

func TestEventIDIsDeterministic(t *testing.T) {
	at := time.Date(2026, 10, 3, 2, 0, 57, 0, time.UTC)
	a := eventID("o_1", at, []int64{1, 2, 3})
	if a != eventID("o_1", at, []int64{1, 2, 3}) {
		t.Fatal("the same statement must produce the same id, or it cannot be traced to its ledger row")
	}
	for name, other := range map[string]string{
		"org":    eventID("o_2", at, []int64{1, 2, 3}),
		"period": eventID("o_1", at.Add(time.Second), []int64{1, 2, 3}),
		"counts": eventID("o_1", at, []int64{1, 2, 4}),
	} {
		if other == a {
			t.Errorf("a different %s produced the same id", name)
		}
	}
	if len(a) != 36 {
		t.Errorf("len(eventID) = %d, want 36 (pug_ + 32 hex)", len(a))
	}
}
