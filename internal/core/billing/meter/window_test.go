package meter

import (
	"testing"
	"time"
)

func date(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

func TestWindowFor(t *testing.T) {
	// Periods start at an instant; windows are whole UTC days.
	start := time.Date(2026, 10, 3, 2, 0, 57, 0, time.UTC)
	end := time.Date(2026, 11, 3, 2, 0, 57, 0, time.UTC)
	cases := []struct {
		name         string
		stated, prev *Window
		wantS, wantE time.Time
	}{
		{"first period", nil, nil, date(2026, 10, 3), date(2026, 11, 3)},
		{"contiguous roll", nil, &Window{date(2026, 9, 3), date(2026, 10, 3)}, date(2026, 10, 3), date(2026, 11, 3)},
		{"a gap after a lapse is in no window", nil, &Window{date(2026, 6, 1), date(2026, 7, 1)}, date(2026, 10, 3), date(2026, 11, 3)},
		{"a cutover's overlap stays with the old window", nil, &Window{date(2026, 9, 15), date(2026, 10, 15)}, date(2026, 10, 15), date(2026, 11, 3)},
		{"an overlap past the period's end is an empty window", nil, &Window{date(2026, 10, 1), date(2026, 12, 1)}, date(2026, 11, 3), date(2026, 11, 3)},
		{"a stated period keeps its start", &Window{date(2026, 10, 15), date(2026, 11, 3)}, nil, date(2026, 10, 15), date(2026, 11, 3)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := windowFor(start, end, tc.stated, tc.prev)
			if !got.Start.Equal(tc.wantS) || !got.End.Equal(tc.wantE) {
				t.Fatalf("windowFor = [%s, %s), want [%s, %s)", got.Start, got.End, tc.wantS, tc.wantE)
			}
		})
	}
}

func TestWindowThrough(t *testing.T) {
	w := Window{date(2026, 10, 3), date(2026, 11, 3)}
	if got := w.through(time.Date(2026, 10, 20, 13, 0, 0, 0, time.UTC)); !got.Equal(date(2026, 10, 21)) {
		t.Errorf("through mid-period = %s, want the day after today", got)
	}
	// The renewal day itself belongs to the next window.
	if got := w.through(time.Date(2026, 11, 3, 1, 0, 0, 0, time.UTC)); !got.Equal(date(2026, 11, 3)) {
		t.Errorf("through on the renewal day = %s, want the window's end", got)
	}
	// A cutover's window can start days after its period does: nothing of it is
	// summed until then, so nothing is recorded as summed either.
	if got := w.through(time.Date(2026, 9, 30, 13, 0, 0, 0, time.UTC)); !got.Equal(w.Start) {
		t.Errorf("through before the window begins = %s, want its start", got)
	}
}

// Across a renewal the subscription was live for the whole window; across a change
// of subscription only for what was summed while it was.
func TestDaysFor(t *testing.T) {
	p := period{Window: Window{date(2026, 10, 3), date(2026, 11, 3)}, SummedThrough: date(2026, 10, 11), SubID: "sub_a"}
	if got := p.daysFor("sub_a"); got != p.Window {
		t.Errorf("daysFor its own subscription = %v, want the whole window", got)
	}
	if got := p.daysFor("sub_b"); got != (Window{date(2026, 10, 3), date(2026, 10, 11)}) {
		t.Errorf("daysFor another subscription = %v, want the days summed while it was live", got)
	}
}

func TestContiguous(t *testing.T) {
	prev := Window{date(2026, 9, 3), date(2026, 10, 3)}
	if !prev.contiguousWith(Window{date(2026, 10, 3), date(2026, 11, 3)}) {
		t.Error("a window starting where the previous ended is contiguous")
	}
	if prev.contiguousWith(Window{date(2026, 10, 4), date(2026, 11, 4)}) {
		t.Error("a gap is not contiguous")
	}
}
