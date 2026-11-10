package apperr

// Dashboards domain reasons.
var (
	ReasonDashboardNotFound         = codes.add("DASHBOARD_NOT_FOUND")
	ReasonDashboardTileNotFound     = codes.add("DASHBOARD_TILE_NOT_FOUND")
	ReasonInvalidTileContent        = codes.add("INVALID_TILE_CONTENT")
	ReasonDashboardTileNameConflict = codes.add("DASHBOARD_TILE_NAME_CONFLICT")
	ReasonDashboardChanged          = codes.add("DASHBOARD_CHANGED")
	ReasonDashboardShared           = codes.add("DASHBOARD_SHARED")
	ReasonInvalidTilePosition       = codes.add("INVALID_TILE_POSITION")
	ReasonDashboardTileLimit        = codes.add("DASHBOARD_TILE_LIMIT")
)
