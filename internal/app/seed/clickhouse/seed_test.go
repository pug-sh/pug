package seed

import (
	"testing"
	"time"
)

func TestInferKind(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: "view", want: "page_view"},
		{input: "cart", want: "add_to_cart"},
		{input: "purchase", want: "purchase"},
		{input: "order-123", want: "purchase"},
		{input: "", want: "page_view"},
	}

	for _, tt := range tests {
		if got := inferKind(tt.input); got != tt.want {
			t.Errorf("inferKind(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestIsEventType(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{input: "view", want: true},
		{input: "cart", want: true},
		{input: "purchase", want: true},
		{input: "signup", want: false},
		{input: "order-123", want: false},
	}

	for _, tt := range tests {
		if got := isEventType(tt.input); got != tt.want {
			t.Errorf("isEventType(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestClampOccurTime(t *testing.T) {
	max := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	past := max.Add(-time.Hour)
	future := max.Add(time.Hour)

	if got := clampOccurTime(past, max); !got.Equal(past) {
		t.Fatalf("past time changed: got %v want %v", got, past)
	}
	if got := clampOccurTime(future, max); !got.Equal(max) {
		t.Fatalf("future time not clamped: got %v want %v", got, max)
	}
}

func TestRandomSessionFromPoolNoFutureEvents(t *testing.T) {
	end := time.Now()
	start := end.AddDate(0, -4, 0)
	pool := buildSessionPool(start, end)
	tracker := newUserSessionTracker()

	for range 1000 {
		sess := randomSessionFromPool(pool, start, end, tracker)
		for i, e := range sess {
			if e.occurTime.After(end) {
				t.Fatalf("session[%d]: occur_time %v is after end %v", i, e.occurTime, end)
			}
		}
	}
}
