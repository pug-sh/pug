package templates

import (
	"strings"
	"testing"
)

func TestEmailCSSHasBrandTokensAndMediaQuery(t *testing.T) {
	// Brand button color must be present so premailer can inline it.
	if !strings.Contains(emailCSS, ColorPrimary) {
		t.Fatalf("emailCSS missing primary color %s", ColorPrimary)
	}
	// No OKLCH may leak into email output — clients can't parse it.
	if strings.Contains(emailCSS, "oklch(") {
		t.Fatalf("emailCSS contains oklch(), which email clients cannot render")
	}
	// A responsive rule must exist; premailer keeps media queries in <style>.
	if !strings.Contains(emailCSS, "@media") {
		t.Fatalf("emailCSS missing @media responsive rule")
	}
}

func TestBrandColorConstants(t *testing.T) {
	if ColorPrimary != "#3c68d9" {
		t.Fatalf("ColorPrimary = %s, want #3c68d9", ColorPrimary)
	}
}

// TestEmailCSSContainsAllBrandColors makes the Color* constants load-bearing:
// every declared brand color must appear in the inlined stylesheet, so changing
// one without the other (drift between brand.go and styles.go) fails here.
func TestEmailCSSContainsAllBrandColors(t *testing.T) {
	colors := map[string]string{
		"ColorPrimary":           ColorPrimary,
		"ColorPrimaryForeground": ColorPrimaryForeground,
		"ColorForeground":        ColorForeground,
		"ColorMutedForeground":   ColorMutedForeground,
		"ColorBackground":        ColorBackground,
		"ColorCard":              ColorCard,
		"ColorBorder":            ColorBorder,
		"ColorMutedBackground":   ColorMutedBackground,
	}
	for name, hex := range colors {
		if !strings.Contains(emailCSS, hex) {
			t.Errorf("emailCSS missing %s (%s): brand.go and styles.go have drifted", name, hex)
		}
	}
}
