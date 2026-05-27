package dashboards

import (
	"crypto/sha256"
	"encoding/json"
	"reflect"

	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
)

// TilePayload is the canonical input for a tile write — everything stored on a
// dashboard_tiles row except identity (id) and timestamps. CreateDashboardTile,
// UpdateDashboardTile, and Upsert all accept this. Encode() produces the
// (DB columns, payload_hash) tuple.
type TilePayload struct {
	DisplayName   string
	Description   string
	Content       TileContent
	ViewMode      dashboardsv1.DashboardTileViewMode
	Layouts       []*dashboardsv1.ResponsiveGridLayout
	Compare       dashboardsv1.ComparePeriod
	Thresholds    []*dashboardsv1.ThresholdRule
	Header        *dashboardsv1.TileHeader
	Visualization *dashboardsv1.VisualizationOptions
}

// EncodedTilePayload is the DB-ready projection of a TilePayload. All map and
// slice fields are non-nil so the corresponding NOT NULL jsonb / bytea columns
// receive a valid value.
type EncodedTilePayload struct {
	Kind          TileKind
	ViewMode      string
	InsightQuery  map[string]any
	MarkdownBody  pgtype.Text
	Layouts       map[string]any
	Compare       string
	Thresholds    []byte
	Header        map[string]any
	Visualization map[string]any
	PayloadHash   []byte
}

// Encode prepares the payload for storage: normalizes view_mode + compare,
// translates nested messages to their column-shaped forms, and computes the
// content hash that Upsert uses to short-circuit no-op writes. All errors
// come from proto marshaling.
func (p TilePayload) Encode() (EncodedTilePayload, error) {
	contentEnc, err := p.Content.Encode()
	if err != nil {
		return EncodedTilePayload{}, err
	}
	viewMode := normalizedTileViewModeProto(contentEnc.Kind, p.ViewMode)
	compare := normalizedComparePeriod(p.Compare)

	thresholdsBytes, err := marshalThresholds(p.Thresholds)
	if err != nil {
		return EncodedTilePayload{}, err
	}
	headerMap, err := messageToMap(p.Header)
	if err != nil {
		return EncodedTilePayload{}, err
	}
	visualizationMap, err := messageToMap(p.Visualization)
	if err != nil {
		return EncodedTilePayload{}, err
	}

	hash, err := computeTilePayloadHash(p, viewMode, compare)
	if err != nil {
		return EncodedTilePayload{}, err
	}

	return EncodedTilePayload{
		Kind:          contentEnc.Kind,
		ViewMode:      viewMode.String(),
		InsightQuery:  contentEnc.InsightQuery,
		MarkdownBody:  contentEnc.MarkdownBody,
		Layouts:       LayoutsToMap(p.Layouts),
		Compare:       compare.String(),
		Thresholds:    thresholdsBytes,
		Header:        headerMap,
		Visualization: visualizationMap,
		PayloadHash:   hash,
	}, nil
}

// computeTilePayloadHash returns sha256 of the deterministic-marshaled
// DashboardTileInput built from the already-normalized payload. The id field
// is left empty: the hash represents content, not identity, so a tile produces
// the same hash whether it's being inserted (no id yet) or updated. Upsert's
// short-circuit relies on this invariant — the hash on the stored row equals
// the hash of any incoming payload that round-trips through Encode.
func computeTilePayloadHash(p TilePayload, viewMode dashboardsv1.DashboardTileViewMode, compare dashboardsv1.ComparePeriod) ([]byte, error) {
	input := &dashboardsv1.DashboardTileInput{
		DisplayName:   proto.String(p.DisplayName),
		Description:   proto.String(p.Description),
		Layouts:       p.Layouts,
		ViewMode:      viewMode.Enum(),
		Compare:       compare.Enum(),
		Thresholds:    p.Thresholds,
		Header:        p.Header,
		Visualization: p.Visualization,
	}
	switch c := p.Content.(type) {
	case InsightTile:
		input.Content = &dashboardsv1.DashboardTileInput_Insight{
			Insight: &dashboardsv1.InsightTileContent{Spec: c.Spec},
		}
	case MarkdownTile:
		input.Content = &dashboardsv1.DashboardTileInput_Markdown{
			Markdown: &dashboardsv1.MarkdownTileContent{Body: proto.String(c.Body)},
		}
	}

	bytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(input)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(bytes)
	return sum[:], nil
}

// normalizedTileViewModeProto mirrors normalizedTileViewMode but stays in
// proto-enum space. Markdown tiles always normalize to UNSPECIFIED; insight
// tiles default unknown / UNSPECIFIED values to LINE.
func normalizedTileViewModeProto(kind TileKind, viewMode dashboardsv1.DashboardTileViewMode) dashboardsv1.DashboardTileViewMode {
	switch kind {
	case TileKindInsight:
		switch viewMode {
		case dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE,
			dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_AREA,
			dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_GROUPED,
			dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_BAR_STACKED,
			dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_TABLE,
			dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_KPI:
			return viewMode
		default:
			return dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_LINE
		}
	case TileKindMarkdown:
		return dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED
	default:
		return dashboardsv1.DashboardTileViewMode_DASHBOARD_TILE_VIEW_MODE_UNSPECIFIED
	}
}

// normalizedComparePeriod defaults unknown values to UNSPECIFIED so future
// enum additions on the client can't write invalid strings to the column.
func normalizedComparePeriod(c dashboardsv1.ComparePeriod) dashboardsv1.ComparePeriod {
	switch c {
	case dashboardsv1.ComparePeriod_COMPARE_PERIOD_PRIOR:
		return c
	default:
		return dashboardsv1.ComparePeriod_COMPARE_PERIOD_UNSPECIFIED
	}
}

// ComparePeriodFromDB returns the enum for a DB-stored proto enum name,
// defaulting unknown / UNSPECIFIED back to UNSPECIFIED.
func ComparePeriodFromDB(name string) dashboardsv1.ComparePeriod {
	value, ok := dashboardsv1.ComparePeriod_value[name]
	if !ok {
		return dashboardsv1.ComparePeriod_COMPARE_PERIOD_UNSPECIFIED
	}
	return normalizedComparePeriod(dashboardsv1.ComparePeriod(value))
}

// MarshalThresholds is exported so the handler-side encoder can use the same
// serialization path the service uses for storage. Empty input still produces
// `[]` so the column always receives a valid JSON array (NOT NULL).
func MarshalThresholds(rules []*dashboardsv1.ThresholdRule) ([]byte, error) {
	return marshalThresholds(rules)
}

func marshalThresholds(rules []*dashboardsv1.ThresholdRule) ([]byte, error) {
	if len(rules) == 0 {
		return []byte("[]"), nil
	}
	parts := make([]json.RawMessage, 0, len(rules))
	for _, r := range rules {
		b, err := protojson.Marshal(r)
		if err != nil {
			return nil, err
		}
		parts = append(parts, b)
	}
	return json.Marshal(parts)
}

// UnmarshalThresholds parses the stored jsonb array back into the proto
// repeated field. An empty / nil / `[]` / `null` input yields a nil slice so
// the read path matches the proto3 "absent" default.
func UnmarshalThresholds(data []byte) ([]*dashboardsv1.ThresholdRule, error) {
	if len(data) == 0 {
		return nil, nil
	}
	s := string(data)
	if s == "[]" || s == "null" {
		return nil, nil
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	opts := protojson.UnmarshalOptions{DiscardUnknown: true}
	out := make([]*dashboardsv1.ThresholdRule, 0, len(raw))
	for _, item := range raw {
		var r dashboardsv1.ThresholdRule
		if err := opts.Unmarshal(item, &r); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, nil
}

// MessageToMap protojson-marshals msg, then unmarshals as map[string]any so
// the resulting value can be stored in a jsonb column via sqlc's default
// mapping. Nil input (or a typed-nil pointer wrapped in the interface) maps
// to an empty map — every NOT NULL jsonb column needs a non-nil value.
func MessageToMap(msg proto.Message) (map[string]any, error) {
	return messageToMap(msg)
}

func messageToMap(msg proto.Message) (map[string]any, error) {
	if msg == nil {
		return map[string]any{}, nil
	}
	if v := reflect.ValueOf(msg); v.Kind() == reflect.Pointer && v.IsNil() {
		return map[string]any{}, nil
	}
	data, err := protojson.Marshal(msg)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// MapToMessage is the inverse of MessageToMap. dst must be a non-nil pointer
// to a proto message; an empty map leaves dst at its zero value (proto3 default).
func MapToMessage(data map[string]any, dst proto.Message) error {
	if len(data) == 0 {
		return nil
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(raw, dst)
}
