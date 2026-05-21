package email

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A display name whose byte-length cap falls inside a multi-byte rune must be
// truncated on a rune boundary, never mid-rune (which would emit invalid UTF-8
// into the subject/body).
func TestSanitizeDisplayTruncatesOnRuneBoundary(t *testing.T) {
	in := strings.Repeat("a", 119) + "€" // 119 + 3 bytes = 122, cap is 120
	got := sanitizeDisplay(in)
	if !utf8.ValidString(got) {
		t.Fatalf("sanitizeDisplay produced invalid UTF-8: %q", got)
	}
	if len(got) > 120 {
		t.Fatalf("sanitizeDisplay exceeded 120-byte cap: %d bytes", len(got))
	}
}

func TestSanitizeDisplayStripsControlChars(t *testing.T) {
	got := sanitizeDisplay("a\nb\rc\x00d\x7fe")
	if want := "abcde"; got != want {
		t.Fatalf("sanitizeDisplay = %q, want %q", got, want)
	}
}

func TestSanitizeDisplayPreservesTab(t *testing.T) {
	got := sanitizeDisplay("a\tb")
	if want := "a\tb"; got != want {
		t.Fatalf("sanitizeDisplay = %q, want %q", got, want)
	}
}
