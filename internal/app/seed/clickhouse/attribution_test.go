package seed

import "testing"

// TestApplyAttributionDerivesLikeHandler pins the seeder's enrichment to the SDK
// ingest handler's. applyAttribution routes the seeder's map[string]any through
// the SAME attribution.Derive the server uses, so demo data and production
// traffic must classify identically — but the two paths reach Derive through
// different Source adapters (the handler's eventProps over *PropertyValue vs the
// seeder's seedProps over any), and adapter coercion is the one place they can
// silently diverge. This mirrors the handler's
// TestEnrichAttribution/"full web pageview derives everything" case field for
// field — same input, same expected output — so a coercion drift in seedProps
// (e.g. the bare `.(string)` or the int/int64-only ScreenDims) surfaces here
// instead of shipping demo analytics that disagree with production.
func TestApplyAttributionDerivesLikeHandler(t *testing.T) {
	props := map[string]any{
		"$url":          "https://Shop.PugAndPals.example.com/products/ball?utm_source=google&utm_medium=cpc&utm_term=dog+food",
		"$referrer":     "https://www.google.com/",
		"$locale":       "en_us",
		"$screenWidth":  1920, // int, exactly as devices.go's [][2]int screens feed it
		"$screenHeight": 1080,
		// Server-only keys a client must never steer: applyAttribution strips
		// them before deriving, so these bogus values must not survive.
		"$channel":        "Nonsense",
		"$referrerDomain": "attacker.example",
	}

	applyAttribution(props)

	want := map[string]string{
		"$pathname":       "/products/ball",
		"$hostname":       "shop.pugandpals.example.com",
		"$referrerDomain": "google.com",
		"$channel":        "Paid Search",
		"$screenSize":     "1920x1080",
		"$utmSource":      "google",
		"$utmMedium":      "cpc",
		"$utmTerm":        "dog food",
		"$locale":         "en-US",
	}
	for k, v := range want {
		got, ok := props[k].(string)
		if !ok || got != v {
			t.Errorf("%s = %v, want %q", k, props[k], v)
		}
	}
}
