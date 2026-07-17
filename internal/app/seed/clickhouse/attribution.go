package seed

import (
	"github.com/pug-sh/pug/internal/attribution"
	"github.com/pug-sh/pug/internal/autoprop"
)

// applyAttribution mirrors the SDK ingest handler's enrichAttribution for
// seeded events: it routes the event's auto-properties through the SAME
// attribution.Derive the server uses, so demo data and production traffic
// classify identically (channel taxonomy, referrer blanking, UTM completion,
// locale casing). The seeder writes straight to ClickHouse and never passes
// the handler, so this is the only place seeded events get their derived
// web-analytics keys.
func applyAttribution(props map[string]any) {
	// Server-only keys are never client-set in the seeder, but run the same
	// strip anyway so the two paths cannot drift.
	attribution.StripServerOnly(props)

	out := attribution.Derive(attribution.InputFrom(seedProps(props)))

	// Mirrors the handler: $locale is rewritten in place, and the write below
	// cannot clear a key, so drop the pre-Derive value and let it re-add the
	// normalized one (if any).
	delete(props, attribution.PropLocale)

	for _, p := range out.Pairs() {
		if p.Value != "" {
			props[p.Key] = p.Value
		}
	}
}

// seedProps adapts the seeder's untyped property map to attribution.Source.
type seedProps map[string]any

func (p seedProps) String(key string) string {
	s, _ := p[key].(string)
	return s
}

func (p seedProps) ScreenDims() (int64, int64) {
	return p.int64(autoprop.PropScreenWidth), p.int64(autoprop.PropScreenHeight)
}

func (p seedProps) int64(key string) int64 {
	switch v := p[key].(type) {
	case int:
		return int64(v)
	case int64:
		return v
	default:
		return 0
	}
}
