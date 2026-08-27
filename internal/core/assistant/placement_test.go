package assistant

import (
	"testing"

	"google.golang.org/protobuf/proto"

	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
)

func draftWithPositions(positions []*dashboardsv1.GridPosition) *dashboardsv1.Dashboard {
	tiles := make([]*dashboardsv1.DashboardTile, len(positions))
	for i, pos := range positions {
		tiles[i] = &dashboardsv1.DashboardTile{
			Id: proto.String("t" + string(rune('0'+i))),
			Content: &dashboardsv1.DashboardTile_Markdown{
				Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String("x")},
			},
			Position: pos,
		}
	}
	return &dashboardsv1.Dashboard{DisplayName: proto.String("d"), Tiles: tiles}
}

func gridPos(x, y, w, h int32) *dashboardsv1.GridPosition {
	return &dashboardsv1.GridPosition{
		X: proto.Int32(x), Y: proto.Int32(y), W: proto.Int32(w), H: proto.Int32(h),
	}
}

func TestPlaceBelow_FirstTileAtOrigin(t *testing.T) {
	pos := placeBelow(draftBottom(&dashboardsv1.Dashboard{DisplayName: proto.String("d")}), nil)
	if pos.GetX() != 0 || pos.GetY() != 0 || pos.GetW() != defaultTileWidth || pos.GetH() != defaultTileHeight {
		t.Fatalf("got x=%d y=%d w=%d h=%d", pos.GetX(), pos.GetY(), pos.GetW(), pos.GetH())
	}
}

func TestPlaceBelow_AbsentDraftTreatedAsEmpty(t *testing.T) {
	pos := placeBelow(draftBottom(nil), nil)
	if pos.GetX() != 0 || pos.GetY() != 0 {
		t.Fatalf("got x=%d y=%d", pos.GetX(), pos.GetY())
	}
}

func TestPlaceBelow_AppendsBelowLowestTile(t *testing.T) {
	pos := placeBelow(draftBottom(draftWithPositions([]*dashboardsv1.GridPosition{gridPos(0, 0, 36, 200)})), nil)
	if pos.GetY() != 200 {
		t.Fatalf("y = %d, want 200", pos.GetY())
	}
}

func TestPlaceBelow_UsesLowestEdgeNotLastTile(t *testing.T) {
	pos := placeBelow(draftBottom(draftWithPositions([]*dashboardsv1.GridPosition{
		gridPos(0, 0, 36, 500),
		gridPos(36, 0, 36, 200),
	})), nil)
	if pos.GetY() != 500 {
		t.Fatalf("y = %d, want 500", pos.GetY())
	}
}

// Advancing the cursor by the placed tile's own height is what keeps successive
// placements apart; TestAddTile_TilesInOneTurnDoNotOverlap covers the real path.
func TestPlaceBelow_AdvancedCursorNeverOverlaps(t *testing.T) {
	first := placeBelow(0, nil)
	second := placeBelow(first.GetY()+first.GetH(), nil)
	if second.GetY() < first.GetY()+first.GetH() {
		t.Fatalf("second.y = %d overlaps first (y=%d h=%d)", second.GetY(), first.GetY(), first.GetH())
	}
}

func TestPlaceBelow_HonoursSaneProposedSize(t *testing.T) {
	pos := placeBelow(draftBottom(nil), &dashboardsv1.GridPosition{W: proto.Int32(72), H: proto.Int32(40)})
	if pos.GetW() != 72 || pos.GetH() != 40 {
		t.Fatalf("got w=%d h=%d", pos.GetW(), pos.GetH())
	}
}

func TestPlaceBelow_ClampsOutOfBoundsProposal(t *testing.T) {
	pos := placeBelow(draftBottom(nil), &dashboardsv1.GridPosition{W: proto.Int32(9999), H: proto.Int32(9999)})
	if pos.GetW() != gridColumns {
		t.Fatalf("w = %d, want %d", pos.GetW(), gridColumns)
	}
	if pos.GetH() != maxTileHeight {
		t.Fatalf("h = %d, want %d", pos.GetH(), maxTileHeight)
	}
}

// Real tiles range 16 rows (KPI) to 40 rows (retention table). A bare add_tile
// should render like a normal chart, not a multi-screen placeholder; the clamp
// ceiling sits above real tiles but far below the proto max of 800.
func TestPlaceBelow_DefaultAndCeilingMatchRealTiles(t *testing.T) {
	if h := placeBelow(draftBottom(nil), nil).GetH(); h > 40 {
		t.Fatalf("default h = %d, want <= 40", h)
	}
	clamped := placeBelow(draftBottom(nil), &dashboardsv1.GridPosition{W: proto.Int32(9999), H: proto.Int32(9999)})
	if clamped.GetH() > 80 {
		t.Fatalf("ceiling h = %d, want <= 80", clamped.GetH())
	}
}

func TestPlaceBelow_IgnoresProposedXY(t *testing.T) {
	pos := placeBelow(
		draftBottom(draftWithPositions([]*dashboardsv1.GridPosition{gridPos(0, 0, 36, 200)})),
		&dashboardsv1.GridPosition{X: proto.Int32(40), Y: proto.Int32(0)},
	)
	if pos.GetX() != 0 || pos.GetY() != 200 {
		t.Fatalf("got x=%d y=%d, want x=0 y=200", pos.GetX(), pos.GetY())
	}
}

func TestPlaceBelow_ZeroOrNegativeProposalFallsBackToDefaults(t *testing.T) {
	pos := placeBelow(draftBottom(nil), &dashboardsv1.GridPosition{W: proto.Int32(0), H: proto.Int32(-5)})
	if pos.GetW() != defaultTileWidth || pos.GetH() != defaultTileHeight {
		t.Fatalf("got w=%d h=%d", pos.GetW(), pos.GetH())
	}
}

// The grid_position.complete CEL rule rejects a partial position, so placeBelow
// must always return all four fields set.
func TestPlaceBelow_AlwaysReturnsCompletePosition(t *testing.T) {
	pos := placeBelow(draftBottom(nil), nil)
	if pos.X == nil || pos.Y == nil || pos.W == nil || pos.H == nil {
		t.Fatalf("incomplete position: %+v", pos)
	}
}
