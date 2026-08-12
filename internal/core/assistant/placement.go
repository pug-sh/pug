package assistant

import (
	"google.golang.org/protobuf/proto"

	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
)

// From dashboards.proto GridPosition. The grid is fine-grained — one column/row
// is roughly the ~18px visual gap — so a tile may span the full 72-column width.
// gridColumns/800 are the proto's bare *legal* bounds (w<=72, h<=800), not sane
// sizes — real tiles in the product range from 16 rows (a KPI card) to 40 rows
// (a retention cohort table). defaultTileHeight and maxTileHeight are picked
// relative to that real range, not the proto limit.
const (
	gridColumns       int32 = 72
	maxTileHeight     int32 = 60
	defaultTileWidth  int32 = 36
	defaultTileHeight int32 = 22
)

func clampInt32(n, lo, hi int32) int32 {
	return min(hi, max(lo, n))
}

// placeTile chooses a grid position for a newly added tile.
//
// Append-below placement: every new tile starts at x=0 on the row after the
// lowest existing tile. Chosen because it is the only strategy that cannot
// produce a broken layout — no overlap is representable. "Add three tiles"
// comes out coherent, just tall. Gap-filling would use the horizontal space
// better but needs collision logic, and the model's own x/y suggestions would
// need overlap resolution anyway.
//
// The model's proposed WIDTH and HEIGHT are honoured when sane (clamped to
// bounds) — it knows a table wants full width and a KPI wants to be short. Its
// proposed x/y are ignored; that is what makes overlap unrepresentable.
//
// MUST return a complete position: the `grid_position.complete` CEL rule
// rejects a partial one, so x, y, w and h all have to be set.
func placeTile(draft *dashboardsv1.Dashboard, proposed *dashboardsv1.GridPosition) *dashboardsv1.GridPosition {
	w := defaultTileWidth
	if proposed.GetW() > 0 {
		w = clampInt32(proposed.GetW(), 1, gridColumns)
	}
	h := defaultTileHeight
	if proposed.GetH() > 0 {
		h = clampInt32(proposed.GetH(), 1, maxTileHeight)
	}

	var bottom int32
	for _, tile := range draft.GetTiles() {
		pos := tile.GetPosition()
		if pos == nil {
			continue
		}
		if edge := pos.GetY() + pos.GetH(); edge > bottom {
			bottom = edge
		}
	}

	return &dashboardsv1.GridPosition{
		X: proto.Int32(0),
		Y: proto.Int32(bottom),
		W: proto.Int32(w),
		H: proto.Int32(h),
	}
}
