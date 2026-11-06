package seed

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/proto"

	chseed "github.com/pug-sh/pug/internal/app/seed/clickhouse"
	coredashboards "github.com/pug-sh/pug/internal/core/dashboards"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
)

// TestDemoDashboardTilesValidate is the safety net for the hand-authored demo
// tile specs: it mirrors the dashboard render path (coredashboards.RenderDashboard
// → renderInsightTile), assembling each insight tile's QueryRequest from the
// stored spec plus the dashboard's resolved window + granularity and running the
// exact same protovalidate.Validate the server runs per tile. A bad operator/value
// pairing, a missing aggregation_property, a breakdown on segmentation, or an
// over-cap window would fail here instead of silently rendering as a per-tile
// error_message in the live demo.
func TestDemoDashboardTilesValidate(t *testing.T) {
	// A fixed instant keeps the resolved windows deterministic; only the window
	// span matters to the granularity caps, not the absolute time.
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	dashboards := demoDashboards()
	if len(dashboards) == 0 {
		t.Fatal("demoDashboards() returned no dashboards")
	}

	seenNames := make(map[string]struct{})
	for _, d := range dashboards {
		if d.displayName == "" {
			t.Error("dashboard has empty display name")
		}
		if _, dup := seenNames[d.displayName]; dup {
			t.Errorf("duplicate dashboard display name %q", d.displayName)
		}
		seenNames[d.displayName] = struct{}{}

		if len(d.tiles) == 0 {
			t.Errorf("dashboard %q has no tiles", d.displayName)
		}

		// Resolve the board's effective window the same way the render path does.
		timeRange := coredashboards.ResolveDashboardTimeRangePreset(d.timeRange, nil, now, time.UTC)

		// Per-dashboard display names must be unique for non-empty names (the
		// partial unique index). Empty names (markdown headers) are exempt.
		tileNames := make(map[string]struct{})
		for i, tile := range d.tiles {
			name := tile.Payload.DisplayName
			if name != "" {
				if _, dup := tileNames[name]; dup {
					t.Errorf("dashboard %q: duplicate tile display name %q", d.displayName, name)
				}
				tileNames[name] = struct{}{}
			}

			switch content := tile.Payload.Content.(type) {
			case coredashboards.MarkdownTile:
				if content.Body == "" {
					t.Errorf("dashboard %q tile #%d: markdown tile has empty body", d.displayName, i)
				}
			case coredashboards.InsightTile:
				assembled := &insightsv1.QueryRequest{
					Spec:        content.Spec,
					TimeRange:   timeRange,
					Granularity: d.granularity.Enum(),
					Timezone:    proto.String(""),
				}
				if err := protovalidate.Validate(assembled); err != nil {
					t.Errorf("dashboard %q tile %q (#%d) failed validation: %v",
						d.displayName, name, i, err)
				}
			default:
				t.Errorf("dashboard %q tile #%d: unexpected content type %T", d.displayName, i, content)
			}
		}
	}
}

// TestDemoDashboardTileLayout pins the hand-authored tile geometry against the
// dashboard FE's 72-column fine grid. The seed was originally authored for a
// 12-column grid, which squished every board into the leftmost 1/6 and — because
// the FE clamps any sub-minimum height UP (grid.tsx Math.max(pos.h, minH)) —
// cascaded the tiles into an overlapping pile.
// This guards against that whole class of regression: every tile must carry a
// fully-specified position, sit within the 72-col width at >= the FE's min tile
// width, clear its kind's height floor, and not overlap any sibling on the board.
func TestDemoDashboardTileLayout(t *testing.T) {
	// Floors mirrored from the FE (app .../dashboards/constants.ts + grid.tsx).
	// Heights below the kind floor get clamped UP at render, so storing them
	// here under the floor is the bug we're guarding against.
	const (
		gridCols    = 72 // COLS.lg
		minTileW    = 12 // TILE_MIN_W
		minInsightH = 15 // getKindMinHeight(insight)
		minKpiH     = 9  // KPI tile min
		minMarkdown = 9  // getKindMinHeight(markdown) == TILE_MIN_H
	)

	for _, d := range demoDashboards() {
		type rect struct {
			x, y, w, h int32
			label      string
		}
		rects := make([]rect, 0, len(d.tiles))
		for i, tile := range d.tiles {
			label := tile.Payload.DisplayName
			if label == "" {
				label = fmt.Sprintf("tile #%d", i)
			}

			pos := tile.Payload.Position
			if pos == nil {
				t.Errorf("dashboard %q %s: nil position", d.displayName, label)
				continue
			}
			// The proto contract: x/y >= 0, w in [1,72], h in [1,800], w & h set.
			if err := protovalidate.Validate(pos); err != nil {
				t.Errorf("dashboard %q %s: invalid position: %v", d.displayName, label, err)
				continue
			}

			x, y, w, h := pos.GetX(), pos.GetY(), pos.GetW(), pos.GetH()
			if w < minTileW {
				t.Errorf("dashboard %q %s: width %d below FE min tile width %d", d.displayName, label, w, minTileW)
			}
			if x+w > gridCols {
				t.Errorf("dashboard %q %s: x+w=%d exceeds grid width %d", d.displayName, label, x+w, gridCols)
			}

			floor := int32(minInsightH)
			switch tile.Payload.Content.(type) {
			case coredashboards.MarkdownTile:
				floor = minMarkdown
			case coredashboards.InsightTile:
				if tile.Payload.ViewMode == viewKPI {
					floor = minKpiH
				}
			}
			if h < floor {
				t.Errorf("dashboard %q %s: height %d below kind floor %d (would be clamped up and overlap the tile below)",
					d.displayName, label, h, floor)
			}

			// Overlap check against every prior tile on this board. Two axis-aligned
			// rects overlap iff they overlap on both axes.
			for _, r := range rects {
				if x < r.x+r.w && r.x < x+w && y < r.y+r.h && r.y < y+h {
					t.Errorf("dashboard %q: tile %s [x=%d y=%d w=%d h=%d] overlaps %s [x=%d y=%d w=%d h=%d]",
						d.displayName, label, x, y, w, h, r.label, r.x, r.y, r.w, r.h)
				}
			}
			rects = append(rects, rect{x: x, y: y, w: w, h: h, label: label})
		}
	}
}

// TestDemoTilesReadGeneratedData fails a tile whose event, property or filter
// value the generator never emits, which would render empty on the demo.
func TestDemoTilesReadGeneratedData(t *testing.T) {
	type seen map[string]map[string]map[string]bool // kind, property, values
	auto, custom := seen{}, seen{}
	add := func(s seen, kind string, props map[string]any) {
		if s[kind] == nil {
			s[kind] = map[string]map[string]bool{}
		}
		for p, v := range props {
			if s[kind][p] == nil {
				s[kind][p] = map[string]bool{}
			}
			s[kind][p][fmt.Sprint(v)] = true
		}
	}
	g := chseed.NewLiveGenerator()
	start := time.Now().AddDate(0, 0, -60)
	for i := range 80_000 {
		now := start.Add(time.Duration(i) * time.Minute)
		sess := g.LiveSession(now)
		if i%20 == 0 {
			sess = append(sess, g.LiveBotSession(now)...)
		}
		for _, e := range sess {
			add(auto, e.Kind, e.AutoProperties)
			add(custom, e.Kind, e.CustomProperties)
		}
	}

	has := func(kinds []string, src commonv1.PropertySource, prop, value string) bool {
		sources := []seen{auto, custom}
		switch src {
		case srcAuto:
			sources = sources[:1]
		case srcCustom:
			sources = sources[1:]
		case srcProfile:
			return false
		}
		for _, k := range kinds {
			for _, s := range sources {
				if vals, ok := s[k][prop]; ok && (value == "" || vals[value]) {
					return true
				}
			}
		}
		return false
	}
	profileHas := func(f *commonv1.PropertyFilter) bool {
		for i := range chseed.DistinctIDPool {
			props, _ := DemoProfileProperties(i)
			v, ok := props[f.GetProperty()]
			if !ok {
				continue
			}
			if f.GetOperator() != opBetween {
				return true
			}
			n, _ := strconv.ParseFloat(fmt.Sprint(v), 64)
			lo, _ := strconv.ParseFloat(f.GetValues()[0], 64)
			hi, _ := strconv.ParseFloat(f.GetValues()[1], 64)
			if n >= lo && n <= hi {
				return true
			}
		}
		return false
	}

	for _, d := range demoDashboards() {
		for _, tile := range d.tiles {
			content, ok := tile.Payload.Content.(coredashboards.InsightTile)
			if !ok {
				continue
			}
			spec := content.Spec
			name := d.displayName + " / " + tile.Payload.DisplayName
			need := func(kinds []string, src commonv1.PropertySource, prop, value string) {
				if !has(kinds, src, prop, value) {
					t.Errorf("%s: no %v event has %s %q", name, kinds, prop, value)
				}
			}
			checkFilter := func(kinds []string, f *commonv1.PropertyFilter) {
				switch {
				case f.GetSource() == srcProfile:
					if !profileHas(f) {
						t.Errorf("%s: no profile matches %s", name, f.GetProperty())
					}
				case f.GetOperator() == opEquals:
					need(kinds, f.GetSource(), f.GetProperty(), f.GetValue())
				case f.GetOperator() == opIn:
					for _, v := range f.GetValues() {
						need(kinds, f.GetSource(), f.GetProperty(), v)
					}
				default:
					need(kinds, f.GetSource(), f.GetProperty(), "")
				}
			}

			var kinds []string
			for _, q := range spec.GetEvents() {
				kinds = append(kinds, q.GetEvent().GetKind())
			}
			var scopes []*commonv1.EventFilter
			for _, sc := range []*commonv1.EventFilter{spec.GetTopK().GetScope(), spec.GetMap().GetScope(), spec.GetUserFlow().GetScope()} {
				if sc != nil {
					scopes = append(scopes, sc)
					kinds = append(kinds, sc.GetKind())
				}
			}
			for _, k := range kinds {
				if _, ok := auto[k]; !ok {
					t.Errorf("%s: the generator emits no %s events", name, k)
				}
			}

			for _, q := range spec.GetEvents() {
				k := []string{q.GetEvent().GetKind()}
				for _, f := range q.GetEvent().GetFilters() {
					checkFilter(k, f)
				}
				if p := q.GetAggregationProperty(); p != "" {
					need(k, 0, p, "")
				}
			}
			for _, sc := range scopes {
				for _, f := range sc.GetFilters() {
					checkFilter(kinds, f)
				}
			}
			for _, fg := range spec.GetFilterGroups() {
				for _, f := range fg.GetFilters() {
					checkFilter(kinds, f)
				}
			}
			for _, b := range spec.GetBreakdowns() {
				need(kinds, 0, b.GetProperty(), "")
			}
			for _, p := range []string{spec.GetTopK().GetProperty(), spec.GetTopK().GetMetricProperty(), spec.GetMap().GetMetricProperty(), spec.GetUserFlow().GetNodeProperty()} {
				if p != "" {
					need(kinds, 0, p, "")
				}
			}
		}
	}
}

// TestDemoDashboardTilesEncode pins that every tile also survives the storage
// encode path (the (columns, payload_hash) projection Upsert persists) — the
// step SeedDemoDashboards relies on. Catches a tile that validates as a query
// but can't be marshaled for storage.
func TestDemoDashboardTilesEncode(t *testing.T) {
	for _, d := range demoDashboards() {
		for i, tile := range d.tiles {
			if _, err := tile.Payload.Encode(); err != nil {
				t.Errorf("dashboard %q tile #%d encode failed: %v", d.displayName, i, err)
			}
		}
	}
}
