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

// TestFontStacksMatchDesignSystem keeps the email font stacks anchored to the
// app's design system (--font-sans / --font-mono in src/index.css): the brand
// faces must lead each stack, and the pre-design-system "Geist" placeholder must
// never creep back in. The named faces don't load in most email clients, but
// leading with them documents the intent and lets web-font-capable clients track
// the app.
func TestFontStacksMatchDesignSystem(t *testing.T) {
	if !strings.HasPrefix(FontSans, `"Apfel Grotezk"`) {
		t.Errorf("FontSans must lead with the app brand face %q, got %s", "Apfel Grotezk", FontSans)
	}
	if !strings.HasPrefix(FontMono, `"JetBrains Mono"`) {
		t.Errorf("FontMono must lead with the app mono face %q, got %s", "JetBrains Mono", FontMono)
	}
	if strings.Contains(emailCSS, "Geist") {
		t.Errorf("emailCSS references Geist, which is not in the app design system")
	}
	if !strings.Contains(emailCSS, "Apfel Grotezk") {
		t.Errorf("emailCSS missing the app brand face (Apfel Grotezk); styles.go and brand.go have drifted")
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
