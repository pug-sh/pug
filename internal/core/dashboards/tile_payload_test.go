package dashboards

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
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
		Compare:  dashboardsv1.ComparePeriod_COMPARE_PERIOD_PRIOR,
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
		Position: &dashboardsv1.GridPosition{
			X: proto.Int32(1), Y: proto.Int32(2),
			W: proto.Int32(4), H: proto.Int32(3),
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

// Position: nil and Position: &GridPosition{} both store as `{}` bytes, so they
// must hash identically — same nil-vs-empty contract as header/visualization.
func TestTilePayload_HashStableAcrossNilVsEmptyPosition(t *testing.T) {
	p := fullPayload()
	p.Position = nil
	hNil := mustEncode(t, p).PayloadHash

	p.Position = &dashboardsv1.GridPosition{}
	hEmpty := mustEncode(t, p).PayloadHash

	if !bytes.Equal(hNil, hEmpty) {
		t.Errorf("hash distinguishes nil from zero-valued position:\n  nil=%x\n  empty=%x", hNil, hEmpty)
	}
}

// Encode must serialize position to the stored jsonb map, and a tile echoed back
// from storage (map decoded via MapToMessage) must re-hash identically — the
// Get→Upsert no-op short-circuit relies on it.
func TestTilePayload_PositionEchoStableThroughStoredMap(t *testing.T) {
	p := fullPayload()
	enc := mustEncode(t, p)
	if len(enc.Position) == 0 {
		t.Fatal("Encode did not populate the Position map")
	}

	var readBack dashboardsv1.GridPosition
	if err := MapToMessage(enc.Position, &readBack); err != nil {
		t.Fatalf("MapToMessage: %v", err)
	}
	echoed := fullPayload()
	echoed.Position = &readBack
	if !bytes.Equal(enc.PayloadHash, mustEncode(t, echoed).PayloadHash) {
		t.Error("position did not echo-stably round-trip through the stored map")
	}
}

// MapStoredMessage surfaces schema drift: a non-empty stored map whose keys
// don't match the target message decodes to a zero value (DiscardUnknown), and
// must warn once per column rather than silently zeroing the field on read.
func TestMapStoredMessage_SurfacesSchemaDrift(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	var pos dashboardsv1.GridPosition
	// "breakpoint" is a stale layouts-shaped key — unknown to GridPosition.
	if err := MapStoredMessage(context.Background(), "test.drift_surfaces", map[string]any{"breakpoint": "lg"}, &pos); err != nil {
		t.Fatalf("MapStoredMessage: %v", err)
	}
	if !proto.Equal(&pos, &dashboardsv1.GridPosition{}) {
		t.Errorf("expected zero position from all-unknown map, got %v", &pos)
	}
	if !strings.Contains(buf.String(), "schema drift") {
		t.Errorf("expected schema-drift warning, got log: %q", buf.String())
	}
}

func TestMapStoredMessage_NoDriftForValidOrEmpty(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	var pos dashboardsv1.GridPosition
	if err := MapStoredMessage(context.Background(), "test.drift_valid", map[string]any{"w": 4, "h": 3}, &pos); err != nil {
		t.Fatalf("MapStoredMessage valid: %v", err)
	}
	if pos.GetW() != 4 || pos.GetH() != 3 {
		t.Errorf("valid message did not decode: %v", &pos)
	}
	// Empty input is the legitimate "absent" case, not drift.
	var empty dashboardsv1.GridPosition
	if err := MapStoredMessage(context.Background(), "test.drift_empty", map[string]any{}, &empty); err != nil {
		t.Fatalf("MapStoredMessage empty: %v", err)
	}
	if strings.Contains(buf.String(), "schema drift") {
		t.Errorf("unexpected drift warning for valid/empty input: %q", buf.String())
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
		{"Header.Borderless", func(p *TilePayload) { p.Header.Borderless = proto.Bool(true) }},
		{"Visualization.LogScale", func(p *TilePayload) { p.Visualization.LogScale = proto.Bool(false) }},
		{"Visualization.HideSparkline", func(p *TilePayload) { p.Visualization.HideSparkline = proto.Bool(true) }},
		{"InsightSpec", func(p *TilePayload) {
			p.Content = InsightTile{Spec: &insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_FUNNEL.Enum(),
			}}
		}},
		{"Markdown swap", func(p *TilePayload) {
			p.Content = MarkdownTile{Body: "now markdown"}
		}},
		{"Position move", func(p *TilePayload) {
			p.Position = &dashboardsv1.GridPosition{
				X: proto.Int32(9), Y: proto.Int32(9), W: proto.Int32(5), H: proto.Int32(5),
			}
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

// TestTilePayload_HashContractCoversEveryInputField is a contract test for the
// hash-subject double-duty risk: computeTilePayloadHash builds a
// DashboardTileInput explicitly, so a field added to that message in proto
// without a corresponding addition here would be silently excluded from the
// hash domain. For each currently-known field on DashboardTileInput, this test
// asserts that populating only that field (and leaving the rest zero) produces
// a hash distinct from the all-zero payload's hash. If the test starts failing
// after a proto edit because a new field is added with no hash coverage, that
// is the contract regression.
func TestTilePayload_HashContractCoversEveryInputField(t *testing.T) {
	zero := TilePayload{Content: MarkdownTile{Body: "x"}}
	zeroHash := mustEncode(t, zero).PayloadHash

	cases := []struct {
		name   string
		mut    func(*TilePayload)
		fields []string // proto fields that this case exercises
	}{
		{"display_name", func(p *TilePayload) { p.DisplayName = "name" }, []string{"display_name"}},
		{"description", func(p *TilePayload) { p.Description = "desc" }, []string{"description"}},
		{"content/markdown", func(p *TilePayload) { p.Content = MarkdownTile{Body: "different"} }, []string{"markdown"}},
		{"content/insight", func(p *TilePayload) {
			p.Content = InsightTile{Spec: &insightsv1.InsightQuerySpec{InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum()}}
		}, []string{"insight"}},
		{"view_mode", func(p *TilePayload) {
			p.Content = InsightTile{Spec: &insightsv1.InsightQuerySpec{InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum()}}
			p.ViewMode = dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_AREA
		}, []string{"view_mode"}},
		{"compare", func(p *TilePayload) { p.Compare = dashboardsv1.ComparePeriod_COMPARE_PERIOD_PRIOR }, []string{"compare"}},
		{"thresholds", func(p *TilePayload) {
			p.Thresholds = []*dashboardsv1.ThresholdRule{{Operator: dashboardsv1.ThresholdRule_OPERATOR_GT.Enum(), Value: proto.Float64(1), Tone: dashboardsv1.ThresholdRule_TONE_GOOD.Enum()}}
		}, []string{"thresholds"}},
		{"header", func(p *TilePayload) { p.Header = &dashboardsv1.TileHeader{Icon: proto.String("📈")} }, []string{"header"}},
		{"visualization", func(p *TilePayload) {
			p.Visualization = &dashboardsv1.VisualizationOptions{LogScale: proto.Bool(true)}
		}, []string{"visualization"}},
		{"position", func(p *TilePayload) {
			p.Position = &dashboardsv1.GridPosition{X: proto.Int32(1), Y: proto.Int32(1), W: proto.Int32(2), H: proto.Int32(2)}
		}, []string{"position"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := TilePayload{Content: MarkdownTile{Body: "x"}}
			tc.mut(&p)
			got := mustEncode(t, p).PayloadHash
			if bytes.Equal(got, zeroHash) {
				t.Errorf("populating %q produced the zero-payload hash — field is not in the hash domain", tc.fields)
			}
		})
	}

	// Sentinel: enumerate the proto descriptor's top-level fields and report
	// any that don't appear in the case list above. Catches drift the other
	// direction — a new proto field whose hash status nobody decided.
	desc := (&dashboardsv1.DashboardTileInput{}).ProtoReflect().Descriptor()
	covered := map[string]bool{
		// id is intentionally excluded — hash represents content, not identity.
		"id": true,
	}
	for _, tc := range cases {
		for _, f := range tc.fields {
			covered[f] = true
		}
	}
	fields := desc.Fields()
	for i := 0; i < fields.Len(); i++ {
		name := string(fields.Get(i).Name())
		if !covered[name] {
			t.Errorf("DashboardTileInput field %q is not covered by the hash contract test — decide whether it should affect the hash and update either computeTilePayloadHash or this test", name)
		}
	}
}
