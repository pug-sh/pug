// Package templates holds the typed templ components and frozen design tokens
// for transactional emails. Tokens are converted once from cotton-w's OKLCH
// design system (src/index.css) to hex, because no email client understands
// oklch(). These hex values are the source of truth for email styling.
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

// Palette — frozen hex from the cotton-w light-theme OKLCH tokens.
const (
	ColorPrimary           = "#3c68d9" // brand: buttons, links, accents
	ColorPrimaryForeground = "#f7f8fc" // button text
	ColorForeground        = "#070b14" // headings, body text
	ColorMutedForeground   = "#6b727e" // secondary text, footer
	ColorBackground        = "#f7f8fa" // email canvas
	ColorCard              = "#ffffff" // content card
	ColorBorder            = "#d4d8de" // dividers, card border
	ColorMutedBackground   = "#e7ebf2" // code / fallback-URL chip
)

// Font stacks. Geist will not load in most clients; the system fallback is
// what actually renders.
const (
	FontSans = `"Geist", -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif`
	FontMono = `ui-monospace, SFMono-Regular, "JetBrains Mono", Menlo, monospace`
)
