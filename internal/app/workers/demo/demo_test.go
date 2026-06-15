package demo

import "testing"

// TestEnabled pins the boolean parsing of PUG_DEMO_ENABLED, which gates whether
// `pug dev` auto-starts the demo worker. Note that only Go bool literals enable
// it: a human-friendly "yes" is intentionally treated as disabled.
func TestEnabled(t *testing.T) {
	tests := []struct {
		val  string
		want bool
	}{
		{"true", true},
		{"1", true},
		{"t", true},
		{"TRUE", true},
		{"false", false},
		{"0", false},
		{"", false},
		{"yes", false},
		{"on", false},
	}
	for _, tt := range tests {
		t.Run(tt.val, func(t *testing.T) {
			t.Setenv("PUG_DEMO_ENABLED", tt.val)
			if got := Enabled(); got != tt.want {
				t.Errorf("Enabled() with PUG_DEMO_ENABLED=%q = %v, want %v", tt.val, got, tt.want)
			}
		})
	}
}
