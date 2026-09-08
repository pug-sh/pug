package dodo

import (
	dodopayments "github.com/dodopayments/dodopayments-go"
	corebilling "github.com/pug-sh/pug/internal/core/billing"
)

// Pug's own tokens, straight off src/index.css in the dashboard — the overlay opens
// over that page, so a mismatch is visible side by side. The font is deliberately
// absent: font_primary_url must be a public https URL, and Figtree ships hashed.
func customization(theme corebilling.CheckoutTheme) dodopayments.CheckoutSessionCustomizationParam {
	c := dodopayments.CheckoutSessionCustomizationParam{
		ThemeConfig: dodopayments.F(dodopayments.ThemeConfigParam{
			Radius: dodopayments.F("0.625rem"),
			Light: dodopayments.F(dodopayments.ThemeModeConfigParam{
				BgPrimary:            dodopayments.F("oklch(0.943 0.005 265)"),
				BgSecondary:          dodopayments.F("oklch(0.958 0.004 265)"),
				BorderPrimary:        dodopayments.F("oklch(0.876 0.006 265)"),
				BorderSecondary:      dodopayments.F("oklch(0.911 0.008 265)"),
				ButtonPrimary:        dodopayments.F("oklch(0.55 0.18 265)"),
				ButtonPrimaryHover:   dodopayments.F("oklch(0.50 0.18 265)"),
				ButtonSecondary:      dodopayments.F("oklch(0.911 0.008 265)"),
				ButtonSecondaryHover: dodopayments.F("oklch(0.876 0.006 265)"),
				ButtonTextPrimary:    dodopayments.F("oklch(0.98 0.005 265)"),
				ButtonTextSecondary:  dodopayments.F("oklch(0.402 0.008 265)"),
				InputFocusBorder:     dodopayments.F("oklch(0.55 0.18 265)"),
				TextPrimary:          dodopayments.F("oklch(0.365 0.006 265)"),
				TextSecondary:        dodopayments.F("oklch(0.49 0.008 265)"),
				TextPlaceholder:      dodopayments.F("oklch(0.611 0.008 265)"),
				TextError:            dodopayments.F("oklch(0.462 0.14 25)"),
				TextSuccess:          dodopayments.F("oklch(0.431 0.14 145)"),
			}),
			Dark: dodopayments.F(dodopayments.ThemeModeConfigParam{
				BgPrimary:            dodopayments.F("oklch(0.215 0.013 265)"),
				BgSecondary:          dodopayments.F("oklch(0.242 0.013 265)"),
				BorderPrimary:        dodopayments.F("oklch(0.68 0.022 265 / 0.22)"),
				BorderSecondary:      dodopayments.F("oklch(0.252 0.014 265)"),
				ButtonPrimary:        dodopayments.F("oklch(0.55 0.175 265)"),
				ButtonPrimaryHover:   dodopayments.F("oklch(0.60 0.175 265)"),
				ButtonSecondary:      dodopayments.F("oklch(0.252 0.014 265)"),
				ButtonSecondaryHover: dodopayments.F("oklch(0.285 0.016 265)"),
				ButtonTextPrimary:    dodopayments.F("oklch(0.985 0.005 265)"),
				ButtonTextSecondary:  dodopayments.F("oklch(0.779 0.005 265)"),
				InputFocusBorder:     dodopayments.F("oklch(0.62 0.15 265)"),
				TextPrimary:          dodopayments.F("oklch(0.818 0.004 265)"),
				TextSecondary:        dodopayments.F("oklch(0.709 0.006 265)"),
				TextPlaceholder:      dodopayments.F("oklch(0.605 0.007 265)"),
				TextError:            dodopayments.F("oklch(0.7 0.1 25)"),
				TextSuccess:          dodopayments.F("oklch(0.71 0.1 145)"),
			}),
		}),
	}
	switch theme {
	case corebilling.CheckoutThemeLight:
		c.Theme = dodopayments.F(dodopayments.CheckoutSessionCustomizationThemeLight)
	case corebilling.CheckoutThemeDark:
		c.Theme = dodopayments.F(dodopayments.CheckoutSessionCustomizationThemeDark)
	case corebilling.CheckoutThemeAuto:
		// No theme at all, which leaves the mode configured in Dodo's own dashboard.
		// Not "system": that follows the buyer's OS, which is a different claim.
	}
	return c
}
