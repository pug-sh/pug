package seed

import (
	"reflect"
	"testing"

	chseed "github.com/pug-sh/pug/internal/app/seed/clickhouse"
)

// TestDemoProfilePropertiesDeterministic pins the determinism the live profile
// creation relies on: the same user index always yields byte-identical
// properties and external id, so re-creating an already-backfilled user via the
// demo worker's live insert leaves its stored properties unchanged under the
// ReplacingMergeTree rather than flipping them.
func TestDemoProfilePropertiesDeterministic(t *testing.T) {
	for _, i := range []int{0, 1, 42, 100, 999, 5000, chseed.DistinctIDPool - 1} {
		du := chseed.DemoUserAt(i)
		p1, e1 := DemoProfileProperties(i)
		p2, e2 := DemoProfileProperties(i)

		if e1 != e2 || !reflect.DeepEqual(p1, p2) {
			t.Errorf("user %d not deterministic:\n (%v, %q)\n (%v, %q)", i, p1, e1, p2, e2)
		}
		// Properties stay aligned with the user's event-side identity.
		if p1["city"] != du.City || p1["country"] != du.Country {
			t.Errorf("user %d: props city/country = %v/%v, want %v/%v", i, p1["city"], p1["country"], du.City, du.Country)
		}
		if du.Member && p1["pug_club"] != true {
			t.Errorf("user %d: member but pug_club not set", i)
		}
		if !du.Member {
			if _, ok := p1["pug_club"]; ok {
				t.Errorf("user %d: non-member has pug_club set", i)
			}
		}
	}
}

// TestDemoProfilePropertiesIdentifiedShare sanity-checks the identified split
// (externalID != "") stays near the intended ~60% across the pool, so the demo
// keeps a realistic mix of identified and anonymous-only profiles.
func TestDemoProfilePropertiesIdentifiedShare(t *testing.T) {
	const n = 10000
	identified := 0
	for i := range n {
		if _, e := DemoProfileProperties(i); e != "" {
			identified++
		}
	}
	share := float64(identified) / float64(n)
	if share < 0.55 || share > 0.65 {
		t.Errorf("identified share = %.3f, want ~0.60", share)
	}
}
