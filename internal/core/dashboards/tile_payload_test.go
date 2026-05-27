package dashboards

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/proto"

	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
)

// fullPayload returns a TilePayload with every customization field populated.
// Used as the baseline for hash-difference assertions.
func fullPayload() TilePayload {
	return TilePayload{
		DisplayName: "Card",
		Description: "blurb",
		Content: InsightTile{Spec: &insightsv1.InsightQuerySpec{
			InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
		}},
		ViewMode: dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE,
		Layouts: []*dashboardsv1.ResponsiveGridLayout{
			{Breakpoint: proto.String("md"), W: proto.Int32(2), H: proto.Int32(2)},
			{Breakpoint: proto.String("lg"), W: proto.Int32(4), H: proto.Int32(3)},
			{Breakpoint: proto.String("xl"), W: proto.Int32(6), H: proto.Int32(4)},
		},
		Compare: dashboardsv1.ComparePeriod_COMPARE_PERIOD_PRIOR,
		Thresholds: []*dashboardsv1.ThresholdRule{
			{Operator: dashboardsv1.ThresholdRule_OPERATOR_GTE.Enum(), Value: proto.Float64(0.5), Tone: dashboardsv1.ThresholdRule_TONE_GOOD.Enum()},
		},
		Header: &dashboardsv1.TileHeader{
			Icon:        proto.String("📈"),
			AccentColor: proto.String("blue"),
		},
		Visualization: &dashboardsv1.VisualizationOptions{
			YAxisFormat: dashboardsv1.VisualizationOptions_Y_AXIS_FORMAT_PERCENT.Enum(),
			LogScale:    proto.Bool(true),
		},
	}
}

func mustEncode(t *testing.T, p TilePayload) EncodedTilePayload {
	t.Helper()
	enc, err := p.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return enc
}

// Pins the cardinal Encode invariant: identical inputs produce identical
// hashes. If this fails, the SQL `payload_hash <> $1` short-circuit becomes
// non-deterministic — the same content can be UPDATEd or no-op on different
// runs.
func TestTilePayload_HashStableAcrossRewrites(t *testing.T) {
	a := mustEncode(t, fullPayload())
	b := mustEncode(t, fullPayload())
	if !bytes.Equal(a.PayloadHash, b.PayloadHash) {
		t.Errorf("hash differs across encodes of identical payload:\n  a=%x\n  b=%x", a.PayloadHash, b.PayloadHash)
	}
}

// Regression for the bug where layouts were hashed in client-supplied order
// while the read path returned them in alphabetical order — every echo-back
// Upsert would then fire a spurious UPDATE. The normalization in
// computeTilePayloadHash sorts before hashing, so input order must not matter.
func TestTilePayload_HashStableAcrossLayoutOrdering(t *testing.T) {
	p1 := fullPayload()
	p2 := fullPayload()
	// Reverse the layout order on p2.
	p2.Layouts = []*dashboardsv1.ResponsiveGridLayout{
		p1.Layouts[2], p1.Layouts[1], p1.Layouts[0],
	}
	h1 := mustEncode(t, p1).PayloadHash
	h2 := mustEncode(t, p2).PayloadHash
	if !bytes.Equal(h1, h2) {
		t.Errorf("hash depends on layout order:\n  in-order=%x\n  reversed=%x", h1, h2)
	}
}

// Regression for the bug where Header: nil and Header: &TileHeader{} produced
// different hashes despite storing as identical `{}` bytes. The normalization
// in computeTilePayloadHash collapses zero-valued nested messages to nil.
func TestTilePayload_HashStableAcrossNilVsEmptyHeader(t *testing.T) {
	p := fullPayload()
	p.Header = nil
	p.Visualization = nil
	hNil := mustEncode(t, p).PayloadHash

	p.Header = &dashboardsv1.TileHeader{}
	p.Visualization = &dashboardsv1.VisualizationOptions{}
	hEmpty := mustEncode(t, p).PayloadHash

	if !bytes.Equal(hNil, hEmpty) {
		t.Errorf("hash distinguishes nil from zero-valued message:\n  nil=%x\n  empty=%x", hNil, hEmpty)
	}
}

// Each customization field is part of the hash domain; mutating any of them
// must produce a different hash. Without this, a future refactor that drops a
// field from the hash input would silently break the short-circuit invariant
// in the "should re-write" direction (no false negatives — but a non-trivial
// change could end up no-op'd in SQL).
func TestTilePayload_HashChangesPerField(t *testing.T) {
	base := mustEncode(t, fullPayload()).PayloadHash

	cases := []struct {
		name   string
		mutate func(*TilePayload)
	}{
		{"DisplayName", func(p *TilePayload) { p.DisplayName = "Other" }},
		{"Description", func(p *TilePayload) { p.Description = "different blurb" }},
		{"ViewMode", func(p *TilePayload) {
			p.ViewMode = dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_STACKED
		}},
		{"Compare", func(p *TilePayload) { p.Compare = dashboardsv1.ComparePeriod_COMPARE_PERIOD_UNSPECIFIED }},
		{"Thresholds", func(p *TilePayload) {
			p.Thresholds = append(p.Thresholds, &dashboardsv1.ThresholdRule{
				Operator: dashboardsv1.ThresholdRule_OPERATOR_LT.Enum(),
				Value:    proto.Float64(0.1),
				Tone:     dashboardsv1.ThresholdRule_TONE_BAD.Enum(),
			})
		}},
		{"Header.Icon", func(p *TilePayload) { p.Header.Icon = proto.String("🔥") }},
		{"Visualization.LogScale", func(p *TilePayload) { p.Visualization.LogScale = proto.Bool(false) }},
		{"InsightSpec", func(p *TilePayload) {
			p.Content = InsightTile{Spec: &insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
			}}
		}},
		{"Markdown swap", func(p *TilePayload) {
			p.Content = MarkdownTile{Body: "now markdown"}
		}},
		{"Layouts add", func(p *TilePayload) {
			p.Layouts = append(p.Layouts, &dashboardsv1.ResponsiveGridLayout{
				Breakpoint: proto.String("sm"), W: proto.Int32(1), H: proto.Int32(1),
			})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := fullPayload()
			tc.mutate(&p)
			got := mustEncode(t, p).PayloadHash
			if bytes.Equal(got, base) {
				t.Errorf("hash did not change after mutating %s", tc.name)
			}
		})
	}
}

// Unknown / out-of-range view_mode values must normalize before hashing so
// the stored hash matches a follow-up Upsert that echoes the normalized value.
func TestTilePayload_HashIgnoresUnknownViewMode(t *testing.T) {
	p := fullPayload()
	p.ViewMode = dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE
	hLine := mustEncode(t, p).PayloadHash

	p.ViewMode = dashboardsv1.DashboardTileViewMode(99) // unknown
	hUnknown := mustEncode(t, p).PayloadHash

	// Both normalize to LINE for an insight tile, so hashes must match.
	if !bytes.Equal(hLine, hUnknown) {
		t.Errorf("unknown ViewMode did not normalize:\n  LINE=%x\n  unknown=%x", hLine, hUnknown)
	}
}

// UnmarshalThresholds tolerates the proto3 "absent" forms (nil, [], null,
// empty) and propagates real JSON / proto errors for genuinely malformed
// data. Pins both halves so a future refactor doesn't accidentally swallow
// corruption.
func TestUnmarshalThresholds_AbsentForms(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
		{"empty array", []byte("[]")},
		{"json null", []byte("null")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := UnmarshalThresholds(tc.data)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if out != nil {
				t.Errorf("expected nil slice, got %v", out)
			}
		})
	}
}

func TestUnmarshalThresholds_MalformedJSON(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{"truncated array", []byte(`[{"operator":"OPERATOR_GTE"`)},
		{"not json at all", []byte("not json")},
		{"non-array root", []byte(`{"operator":"OPERATOR_GTE"}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := UnmarshalThresholds(tc.data); err == nil {
				t.Errorf("expected error for malformed input %q, got nil", tc.data)
			}
		})
	}
}
