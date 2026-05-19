package orgemailproviders

import "testing"

// TestRedactAPIKey pins the 12-char threshold below which the key is fully
// collapsed to "***". Without this, a short-format or test API key could leak
// a meaningful prefix/suffix if anyone changed the threshold.
func TestRedactAPIKey(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "***"},
		{"short", "abc", "***"},
		{"exactly_12_chars", "abcdefghijkl", "***"},
		{"13_chars", "abcdefghijklm", "abcdefgh***jklm"},
		{"long_resend_style", "sk_test_abcdef1234567890", "sk_test_***7890"},
		{"long_with_distinct_suffix", "sk_live_0000000000ZZZZ", "sk_live_***ZZZZ"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactAPIKey(tc.in)
			if got != tc.want {
				t.Fatalf("redactAPIKey(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
