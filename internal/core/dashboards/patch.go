package dashboards

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"buf.build/go/protovalidate"
	"github.com/jackc/pgx/v5"
	"github.com/rs/xid"
	"google.golang.org/protobuf/proto"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
)

// MaxDashboardTiles is the most tiles a dashboard holds, as in the app's Upsert.
const MaxDashboardTiles = 100

// UnitsPerColumn maps the app's 12 display columns onto the stored 72-unit grid.
const UnitsPerColumn = 6

const (
	displayColumns = 12
	gridUnits      = displayColumns * UnitsPerColumn
)

// The shared Position's bounds, which every position Get reports keeps: the MCP
// output schema carries them, and a client rejects a result that breaks them.
const (
	minTileWidth  = 2
	maxTileY      = 100000
	maxTileHeight = 800
)

var (
	ErrDashboardChanged   = errors.New("dashboard changed since it was read")
	ErrDashboardShared    = errors.New("dashboard has sharing on")
	ErrDashboardTileLimit = errors.New("too many tiles on dashboard")
)

// InvalidTileError refuses a tile of a patch. Message names the tile and is
// safe to show the caller.
type InvalidTileError struct{ Message string }

func (e *InvalidTileError) Error() string { return e.Message }

// TilePositionError refuses a position the call gave a tile: shorter than the
// tile's floor, or over another tile. Message names the tiles.
type TilePositionError struct{ Message string }

func (e *TilePositionError) Error() string { return e.Message }

// TileQueryError refuses an insight tile whose query doesn't fit the dashboard's
// window. Message names the tile.
type TileQueryError struct{ Message string }

func (e *TileQueryError) Error() string { return e.Message }

// TileNotFoundError refuses a tile id that isn't on the dashboard.
type TileNotFoundError struct{ ID string }

func (e *TileNotFoundError) Error() string { return "tile " + e.ID + " isn't on the dashboard" }

// PatchDashboardInput creates a dashboard (empty ID) or edits one. A nil field
// keeps the stored value, or takes the default on create.
type PatchDashboardInput struct {
	ID                 string
	UpdateTime         time.Time // the dashboard's update_time when the caller read it
	DisplayName        *string
	Description        *string
	DefaultTimeRange   *commonv1.TimeRangePreset
	DefaultGranularity *insightsv1.Granularity
	Tiles              []PatchTileInput
	Remove             []string
	Timezone           string // the project's reporting_timezone
}

// PatchTileInput adds a tile (empty ID) or changes one, where a nil field or an
// unspecified ViewMode keeps the stored value. Position is on the stored grid.
type PatchTileInput struct {
	ID          string
	DisplayName *string
	Description *string
	Spec        *insightsv1.InsightQuerySpec
	Markdown    *string
	ViewMode    dashboardsv1.DashboardTileViewMode
	Position    *dashboardsv1.GridPosition
}

// PatchDashboardResult names the saved dashboard and, in call order, the tiles
// the call sent.
type PatchDashboardResult struct {
	ID         string
	UpdateTime time.Time
	TileIDs    []string
}

type patchTile struct {
	id      string // empty for a new tile
	payload TilePayload
	label   string // names the tile in errors
	placed  bool   // the call set its position
	queried bool   // the call set its spec
}

// PatchDashboard creates a dashboard or edits one in a single transaction. An
// edit holds the dashboard's row lock and refuses a dashboard with sharing on or
// a stale UpdateTime. Tiles the call leaves out stay, and a changed tile keeps
// every field the call doesn't set.
func (s *Service) PatchDashboard(ctx context.Context, projectID string, in PatchDashboardInput) (result PatchDashboardResult, retErr error) {
	dashboardID := in.ID
	if dashboardID == "" {
		dashboardID = xid.New().String()
	}
	attrs := []any{slog.String("project_id", projectID), slog.String("dashboard_id", dashboardID)}

	tx, err := s.pgW.Begin(ctx)
	if err != nil {
		return PatchDashboardResult{}, recordServiceError(ctx, "failed to begin patch transaction", err, attrs...)
	}
	defer func() {
		if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			warn := append([]any{slogx.Error(rbErr)}, attrs...)
			if retErr != nil {
				warn = append(warn, slog.String("triggering_error", retErr.Error()))
			}
			slog.WarnContext(ctx, "failed to rollback patch transaction", warn...)
		}
	}()
	r := s.read.WithTx(tx)
	w := s.write.WithTx(tx)

	dashboard, stored, err := openForPatchInTx(ctx, r, w, projectID, dashboardID, in)
	if err != nil {
		return PatchDashboardResult{}, err
	}
	storedRange := DashboardDefaultTimeRangePresetFromDB(ctx, dashboard.DefaultTimeRange)
	storedGranularity := DashboardGranularityFromDB(ctx, dashboard.DefaultGranularity)
	meta := UpsertDashboardInput{
		DisplayName:        valueOr(in.DisplayName, dashboard.DisplayName),
		Description:        valueOr(in.Description, dashboard.Description),
		DefaultTimeRange:   normalizedDashboardDefaultTimeRange(valueOr(in.DefaultTimeRange, storedRange)),
		DefaultGranularity: normalizedDashboardGranularity(valueOr(in.DefaultGranularity, storedGranularity)),
	}

	tiles, callIdx, err := mergeTiles(ctx, stored, in)
	if err != nil {
		return PatchDashboardResult{}, err
	}
	if len(tiles) > MaxDashboardTiles {
		return PatchDashboardResult{}, ErrDashboardTileLimit
	}
	placeNewTiles(tiles)
	if err := checkPlacement(tiles); err != nil {
		return PatchDashboardResult{}, err
	}
	timeRange, granularity := resolveEffectiveWindow(ctx, dbread.Dashboard{
		ProjectID:          projectID,
		DefaultTimeRange:   dashboardDefaultTimeRangeDBName(meta.DefaultTimeRange),
		DefaultGranularity: dashboardGranularityDBName(meta.DefaultGranularity),
	}, DashboardQueryOverrides{Timezone: in.Timezone}, time.Now())
	windowChanged := meta.DefaultTimeRange != storedRange || meta.DefaultGranularity != storedGranularity
	if err := checkQueries(tiles, timeRange, granularity, in.Timezone, windowChanged); err != nil {
		return PatchDashboardResult{}, err
	}

	meta.Tiles = make([]UpsertTileInput, len(tiles))
	encoded := make([]EncodedTilePayload, len(tiles))
	for i, t := range tiles {
		enc, err := t.payload.Encode()
		if err != nil {
			return PatchDashboardResult{}, recordServiceError(ctx, "failed to encode tile payload for patch", err,
				append(attrs, slog.Int("tile_index", i))...)
		}
		meta.Tiles[i] = UpsertTileInput{ID: t.id, Payload: t.payload}
		encoded[i] = enc
	}
	finalIDs, err := reconcileTilesInTx(ctx, r, w, projectID, dashboardID, meta, encoded)
	if err != nil {
		return PatchDashboardResult{}, err
	}
	saved, err := r.GetDashboardByIDAndProjectID(ctx, dbread.GetDashboardByIDAndProjectIDParams{ID: dashboardID, ProjectID: projectID})
	if err != nil {
		return PatchDashboardResult{}, recordServiceError(ctx, "failed to read dashboard after patch", err, attrs...)
	}
	if err := tx.Commit(ctx); err != nil {
		return PatchDashboardResult{}, recordServiceError(ctx, "failed to commit patch transaction", err, attrs...)
	}

	tileIDs := make([]string, len(callIdx))
	for i, j := range callIdx {
		tileIDs[i] = finalIDs[j]
	}
	return PatchDashboardResult{ID: dashboardID, UpdateTime: saved.UpdateTime.Time, TileIDs: tileIDs}, nil
}

// openForPatchInTx creates the dashboard, or locks it and loads its tiles.
func openForPatchInTx(ctx context.Context, r *dbread.Queries, w *dbwrite.Queries, projectID, dashboardID string, in PatchDashboardInput) (dbread.Dashboard, []dbread.DashboardTile, error) {
	attrs := []any{slog.String("project_id", projectID), slog.String("dashboard_id", dashboardID)}
	if in.ID == "" {
		created, err := w.CreateDashboard(ctx, dbwrite.CreateDashboardParams{
			ID:                 dashboardID,
			ProjectID:          projectID,
			DisplayName:        valueOr(in.DisplayName, ""),
			Description:        valueOr(in.Description, ""),
			DefaultTimeRange:   dashboardDefaultTimeRangeDBName(valueOr(in.DefaultTimeRange, 0)),
			DefaultGranularity: dashboardGranularityDBName(valueOr(in.DefaultGranularity, 0)),
		})
		if err != nil {
			return dbread.Dashboard{}, nil, recordServiceError(ctx, "failed to create dashboard for patch", err, attrs...)
		}
		return dbwriteToDbread(created), nil, nil
	}

	dashboard, err := r.LockDashboardByIDAndProjectID(ctx, dbread.LockDashboardByIDAndProjectIDParams{ID: dashboardID, ProjectID: projectID})
	if errors.Is(err, pgx.ErrNoRows) {
		return dbread.Dashboard{}, nil, ErrDashboardNotFound
	}
	if err != nil {
		return dbread.Dashboard{}, nil, recordServiceError(ctx, "failed to lock dashboard for patch", err, attrs...)
	}
	share, err := lookupShare(ctx, r, dashboardID)
	if err != nil {
		return dbread.Dashboard{}, nil, err
	}
	if share != nil {
		return dbread.Dashboard{}, nil, ErrDashboardShared
	}
	if !dashboard.UpdateTime.Time.Equal(in.UpdateTime) {
		return dbread.Dashboard{}, nil, ErrDashboardChanged
	}
	tiles, err := r.ListDashboardTilesByDashboardIDAndProjectID(ctx, dbread.ListDashboardTilesByDashboardIDAndProjectIDParams{
		DashboardID: dashboardID,
		ProjectID:   projectID,
	})
	if err != nil {
		return dbread.Dashboard{}, nil, recordServiceError(ctx, "failed to list tiles for patch", err, attrs...)
	}
	return dashboard, tiles, nil
}

// mergeTiles applies the call to the stored tiles: listed ones go, changed ones
// take what the call sets, and new ones follow in call order. callIdx[i] is the
// merged index of in.Tiles[i].
func mergeTiles(ctx context.Context, stored []dbread.DashboardTile, in PatchDashboardInput) ([]patchTile, []int, error) {
	onDashboard := make(map[string]bool, len(stored))
	for _, row := range stored {
		onDashboard[row.ID] = true
	}
	removed := make(map[string]bool, len(in.Remove))
	for _, id := range in.Remove {
		if !onDashboard[id] {
			return nil, nil, &TileNotFoundError{ID: id}
		}
		removed[id] = true
	}
	changes := make(map[string]int, len(in.Tiles))
	for i, c := range in.Tiles {
		if c.ID == "" {
			continue
		}
		if !onDashboard[c.ID] {
			return nil, nil, &TileNotFoundError{ID: c.ID}
		}
		if _, dup := changes[c.ID]; dup || removed[c.ID] {
			return nil, nil, &InvalidTileError{Message: fmt.Sprintf("tile %s appears more than once", c.ID)}
		}
		changes[c.ID] = i
	}

	tiles := make([]patchTile, 0, len(stored)+len(in.Tiles))
	callIdx := make([]int, len(in.Tiles))
	for _, row := range stored {
		if removed[row.ID] {
			continue
		}
		msg, err := TileToRPC(ctx, row)
		if err != nil {
			return nil, nil, recordServiceError(ctx, "failed to decode stored tile for patch", err,
				slog.String("dashboard_id", row.DashboardID), slog.String("tile_id", row.ID))
		}
		t := patchTile{id: row.ID, payload: payloadFromTile(msg), label: "tile " + row.ID}
		if i, ok := changes[row.ID]; ok {
			if err := t.change(in.Tiles[i]); err != nil {
				return nil, nil, err
			}
			callIdx[i] = len(tiles)
		}
		tiles = append(tiles, t)
	}
	for i, c := range in.Tiles {
		if c.ID != "" {
			continue
		}
		t, err := newPatchTile(c, i)
		if err != nil {
			return nil, nil, err
		}
		callIdx[i] = len(tiles)
		tiles = append(tiles, t)
	}
	return tiles, callIdx, nil
}

// payloadFromTile is a stored tile's payload as Get returns it, so re-saving an
// untouched tile reproduces its payload hash.
func payloadFromTile(msg *dashboardsv1.DashboardTile) TilePayload {
	p := TilePayload{
		DisplayName:   msg.GetDisplayName(),
		Description:   msg.GetDescription(),
		ViewMode:      msg.GetViewMode(),
		Compare:       msg.GetCompare(),
		Thresholds:    msg.GetThresholds(),
		Header:        msg.GetHeader(),
		Visualization: msg.GetVisualization(),
		Position:      msg.GetPosition(),
	}
	switch c := msg.GetContent().(type) {
	case *dashboardsv1.DashboardTile_Insight:
		p.Content = InsightTile{Spec: c.Insight.GetSpec()}
	case *dashboardsv1.DashboardTile_Markdown:
		p.Content = MarkdownTile{Body: c.Markdown.GetBody()}
	}
	return p
}

func newPatchTile(c PatchTileInput, i int) (patchTile, error) {
	t := patchTile{
		payload: TilePayload{
			DisplayName: valueOr(c.DisplayName, ""),
			Description: valueOr(c.Description, ""),
			Position:    c.Position,
		},
		label:  fmt.Sprintf("tiles[%d]", i),
		placed: c.Position != nil,
	}
	switch {
	case c.Spec != nil && c.Markdown == nil:
		t.payload.Content = InsightTile{Spec: c.Spec}
		t.queried = true
	case c.Markdown != nil && c.Spec == nil:
		t.payload.Content = MarkdownTile{Body: *c.Markdown}
	default:
		return patchTile{}, &InvalidTileError{Message: t.label + " needs exactly one of spec and markdown"}
	}
	if err := t.setView(c.ViewMode, true); err != nil {
		return patchTile{}, err
	}
	return t, nil
}

func (t *patchTile) change(c PatchTileInput) error {
	p := &t.payload
	if c.DisplayName != nil {
		p.DisplayName = *c.DisplayName
	}
	if c.Description != nil {
		p.Description = *c.Description
	}
	retyped := false
	switch content := p.Content.(type) {
	case InsightTile:
		if c.Markdown != nil {
			return &InvalidTileError{Message: t.label + " is an insight tile and can't take markdown"}
		}
		if c.Spec != nil {
			retyped = c.Spec.GetInsightType() != content.Spec.GetInsightType()
			p.Content = InsightTile{Spec: c.Spec}
			t.queried = true
		}
	case MarkdownTile:
		if c.Spec != nil {
			return &InvalidTileError{Message: t.label + " is a markdown tile and can't take a spec"}
		}
		if c.Markdown != nil {
			p.Content = MarkdownTile{Body: *c.Markdown}
		}
	}
	if c.Position != nil {
		p.Position = c.Position
		t.placed = true
	}
	return t.setView(c.ViewMode, retyped)
}

// chartDefaults are the insight types whose view the call picks, each with the
// view it takes when the call doesn't.
var chartDefaults = map[insightsv1.InsightType]dashboardsv1.DashboardTileViewMode{
	insightsv1.InsightType_INSIGHT_TYPE_TRENDS: dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE,
	insightsv1.InsightType_INSIGHT_TYPE_TOP_K:  dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_TABLE,
}

// fixedViews are the only views the other insight types take, matching the
// demo dashboards.
var fixedViews = map[insightsv1.InsightType]dashboardsv1.DashboardTileViewMode{
	insightsv1.InsightType_INSIGHT_TYPE_FUNNEL:    dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_GROUPED,
	insightsv1.InsightType_INSIGHT_TYPE_RETENTION: dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_TABLE,
	insightsv1.InsightType_INSIGHT_TYPE_USER_FLOW: dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_SANKEY,
	insightsv1.InsightType_INSIGHT_TYPE_MAP:       dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_MAP,
}

// setView applies the call's chart, or gives a new or retyped insight tile its
// type's view. Otherwise the stored view stays.
func (t *patchTile) setView(chart dashboardsv1.DashboardTileViewMode, retyped bool) error {
	insight, isInsight := t.payload.Content.(InsightTile)
	insightType := insight.Spec.GetInsightType()
	def, chartable := chartDefaults[insightType]
	switch {
	case chart != dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED:
		if !isInsight || !chartable {
			return &InvalidTileError{Message: t.label + ": chart applies to trends and top K only"}
		}
		t.payload.ViewMode = chart
	case isInsight && retyped && chartable:
		t.payload.ViewMode = def
	case isInsight && retyped:
		t.payload.ViewMode = fixedViews[insightType]
	}
	return nil
}

// defaultSize is a new tile's size on the stored grid when the call gives none.
func defaultSize(p TilePayload) (w, h int32) {
	insight, ok := p.Content.(InsightTile)
	if !ok {
		return 4 * UnitsPerColumn, 16
	}
	switch insight.Spec.GetInsightType() {
	case insightsv1.InsightType_INSIGHT_TYPE_FUNNEL:
		if len(insight.Spec.GetBreakdowns()) > 0 {
			return 12 * UnitsPerColumn, 51
		}
		return 12 * UnitsPerColumn, 26
	case insightsv1.InsightType_INSIGHT_TYPE_RETENTION:
		return 12 * UnitsPerColumn, 40
	case insightsv1.InsightType_INSIGHT_TYPE_USER_FLOW:
		return 12 * UnitsPerColumn, 32
	case insightsv1.InsightType_INSIGHT_TYPE_MAP:
		return 6 * UnitsPerColumn, 26
	case insightsv1.InsightType_INSIGHT_TYPE_TRENDS,
		insightsv1.InsightType_INSIGHT_TYPE_SEGMENTATION,
		insightsv1.InsightType_INSIGHT_TYPE_TOP_K:
	}
	switch p.ViewMode {
	case dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_KPI:
		return 3 * UnitsPerColumn, 16
	case dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_TABLE:
		return 6 * UnitsPerColumn, 24
	case dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE,
		dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_AREA,
		dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_GROUPED,
		dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_STACKED,
		dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_PIE,
		dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_SANKEY,
		dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_MAP:
	}
	return 6 * UnitsPerColumn, 22
}

// minHeight is the app's floor for a tile's height.
func minHeight(p TilePayload) int32 {
	if _, md := p.Content.(MarkdownTile); md || p.ViewMode == dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_KPI {
		return 9
	}
	return 15
}

type rect struct{ x, y, w, h int32 }

// rect is where Get reports the tile, so the overlap check agrees with what the
// caller read.
func (t *patchTile) rect() rect {
	x, y, w, h := displayPosition(t.payload.Position, minHeight(t.payload))
	return rect{x, y, w, h}
}

// DisplayPosition is where Get reports tile on the app's 12 columns. It rounds x
// and the right edge as the app's storedPositionToDisplay does and grows a short
// tile to its floor, as the app draws it, then keeps the result inside the shared
// Position's bounds. A tile with no position sits at the app's fallback.
func DisplayPosition(tile *dashboardsv1.DashboardTile) (x, y, w, h int32) {
	p := payloadFromTile(tile)
	return displayPosition(p.Position, minHeight(p))
}

func displayPosition(pos *dashboardsv1.GridPosition, floor int32) (x, y, w, h int32) {
	if pos == nil {
		return 0, 0, 6, 18
	}
	// Clamped first so rounding a stored x near the int32 limit can't overflow.
	px, pw := min(pos.GetX(), gridUnits), min(pos.GetW(), gridUnits)
	x = min(max(columns(px), 0), displayColumns-minTileWidth)
	right := min(displayColumns, max(x+minTileWidth, columns(max(px+1, px+pw))))
	y = min(max(pos.GetY(), 0), maxTileY)
	h = min(max(pos.GetH(), floor), maxTileHeight)
	return x, y, right - x, h
}

// columns is Math.round(units / 6) for the non-negative units the app stores.
func columns(units int32) int32 {
	return (units + UnitsPerColumn/2) / UnitsPerColumn
}

func (a rect) overlaps(b rect) bool {
	return a.x < b.x+b.w && b.x < a.x+a.w && a.y < b.y+b.h && b.y < a.y+a.h
}

// placeNewTiles puts each new tile the call didn't place below the lowest tile,
// filling rows left to right. Every tile in a row takes the row's height.
func placeNewTiles(tiles []patchTile) {
	var bottom int32
	var pending []int
	for i, t := range tiles {
		if t.id == "" && t.payload.Position == nil {
			pending = append(pending, i)
			continue
		}
		r := t.rect()
		bottom = max(bottom, r.y+r.h)
	}

	x, y, rowH := int32(0), bottom, int32(0)
	var row []int
	endRow := func() {
		for _, i := range row {
			tiles[i].payload.Position.H = proto.Int32(rowH)
		}
		x, y, rowH, row = 0, y+rowH, 0, row[:0]
	}
	for _, i := range pending {
		w, h := defaultSize(tiles[i].payload)
		if x+w > gridUnits {
			endRow()
		}
		tiles[i].payload.Position = &dashboardsv1.GridPosition{X: proto.Int32(x), Y: proto.Int32(y), W: proto.Int32(w), H: proto.Int32(h)}
		row = append(row, i)
		rowH = max(rowH, h)
		x += w
	}
	endRow()
}

// checkPlacement refuses a tile the call placed that is shorter than its floor
// or overlaps another tile.
func checkPlacement(tiles []patchTile) error {
	for i := range tiles {
		t := &tiles[i]
		if !t.placed {
			continue
		}
		if floor := minHeight(t.payload); t.payload.Position.GetH() < floor {
			return &TilePositionError{Message: fmt.Sprintf("%s: h must be at least %d", t.label, floor)}
		}
		placed := t.rect()
		for j := range tiles {
			if j != i && placed.overlaps(tiles[j].rect()) {
				return &TilePositionError{Message: t.label + " overlaps " + tiles[j].label}
			}
		}
	}
	return nil
}

// checkQueries validates, as QueryDashboard does, the query of each insight tile
// whose spec the call set, or of every insight tile when the window changed.
func checkQueries(tiles []patchTile, timeRange *commonv1.TimeRange, granularity insightsv1.Granularity, timezone string, all bool) error {
	for _, t := range tiles {
		insight, ok := t.payload.Content.(InsightTile)
		if !ok || (!all && !t.queried) {
			continue
		}
		if err := protovalidate.Validate(tileQuery(insight.Spec, timeRange, granularity, timezone)); err != nil {
			return &TileQueryError{Message: fmt.Sprintf("%s doesn't fit the dashboard's window: %v", t.label, err)}
		}
	}
	return nil
}

func valueOr[T any](p *T, fallback T) T {
	if p == nil {
		return fallback
	}
	return *p
}
