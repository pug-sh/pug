// Package templates holds the typed templ components and frozen design tokens
// for transactional emails. Tokens are converted once from the frontend's OKLCH
// design system (src/index.css) to hex, because no email client understands
// oklch().
//
// The values that actually render are the literals inlined in the stylesheet
// (styles.go) and the bgcolor in components.templ. The Color* constants below
// document that palette and are kept in lockstep with the stylesheet by
// TestEmailCSSContainsAllBrandColors in styles_test.go.
package templates

// Brand carries the per-environment branding injected into the layout.
// It lives here (not in core/email) so templ components can reference it
// without importing core/email, which would create an import cycle.
type Brand struct {
	ProductName  string
	LogoURL      string // hosted PNG; empty => wordmark-only header
	DashboardURL string
}

// ProductName is the wordmark shown in the header and footer.
const ProductName = "Pug"

// Palette — the exact sRGB conversion of the app's light-theme OKLCH tokens
// (the CSS var each maps to is named in the trailing comment). Regenerate with
// the same OKLab→sRGB math whenever :root in src/index.css moves.
const (
	ColorPrimary           = "#3c68d9" // --primary: buttons, links, accents
	ColorPrimaryForeground = "#f7f8fc" // --primary-foreground: button text
	ColorForeground        = "#151b24" // --foreground: headings, body text
	ColorMutedForeground   = "#6b727e" // --muted-foreground: secondary text, footer
	ColorBackground        = "#f7f8fa" // --background: email canvas
	ColorCard              = "#fdfdfe" // --card: content card
	ColorBorder            = "#d4d8de" // --border: dividers, card border
	ColorMutedBackground   = "#e7ebf2" // --muted: code / fallback-URL chip
)

// Font stacks, mirroring --font-sans / --font-mono in src/index.css. The named
// brand faces (Apfel Grotezk, JetBrains Mono) are self-hosted and will not load
// in most email clients; the system fallback is what actually renders. Leading
// with them keeps the stack identical to the app so clients that DO honor web
// fonts (e.g. Apple Mail) can track it, and documents the intended face.
const (
	FontSans = `"Apfel Grotezk", system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif`
	FontMono = `"JetBrains Mono", ui-monospace, SFMono-Regular, Menlo, monospace`
)
