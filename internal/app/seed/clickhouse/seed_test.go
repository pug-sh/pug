package seed

import "testing"

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
