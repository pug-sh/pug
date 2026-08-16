package billing

import "testing"

// The real catalog always has both floors, so the guard would otherwise never
// run. Swaps the catalog for its duration, hence living inside the package.
func TestNewServiceRefusesACatalogMissingAFloor(t *testing.T) {
	for _, missing := range []string{SlugFree, SlugTrial} {
		t.Run(missing, func(t *testing.T) {
			original := catalog
			t.Cleanup(func() { catalog = original })

			trimmed := make([]Plan, 0, len(original))
			for _, p := range original {
				if p.Slug != missing {
					trimmed = append(trimmed, p)
				}
			}
			catalog = trimmed

			// Construction never touches the pools, so nil reaches the check.
			if _, err := NewService(nil, nil, true); err == nil {
				t.Fatalf("NewService accepted a catalog with no %q tier", missing)
			}
		})
	}
}
