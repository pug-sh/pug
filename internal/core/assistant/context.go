package assistant

import (
	"fmt"
	"strings"

	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
)

// Enum names are rendered TS-style — the short protobuf-es name ("LINE",
// "TRENDS"), not Go's fully-prefixed one — so the model sees the same summary
// text the TS service produced.
func viewModeName(m dashboardsv1.DashboardTileViewMode) string {
	name, ok := dashboardsv1.DashboardTileViewMode_name[int32(m)]
	if !ok {
		return "UNSPECIFIED"
	}
	return strings.TrimPrefix(name, "DASHBOARD_TILE_VIEW_MODE_")
}

func insightTypeName(t insightsv1.InsightType) string {
	name, ok := insightsv1.InsightType_name[int32(t)]
	if !ok {
		return "UNSPECIFIED"
	}
	return strings.TrimPrefix(name, "INSIGHT_TYPE_")
}

func describeTile(t *dashboardsv1.DashboardTile) string {
	head := fmt.Sprintf("- id=%s %q view=%s", t.GetId(), t.GetDisplayName(), viewModeName(t.GetViewMode()))

	if t.GetMarkdown() != nil {
		return head + " kind=markdown"
	}
	insight := t.GetInsight()
	if insight == nil {
		return head + " kind=empty"
	}
	spec := insight.GetSpec()
	if spec == nil {
		return head + " kind=insight (no spec)"
	}

	eventKinds := make([]string, 0, len(spec.GetEvents()))
	for _, e := range spec.GetEvents() {
		if e.GetEvent() == nil {
			eventKinds = append(eventKinds, "?")
			continue
		}
		eventKinds = append(eventKinds, e.GetEvent().GetKind())
	}
	breakdowns := make([]string, 0, len(spec.GetBreakdowns()))
	for _, b := range spec.GetBreakdowns() {
		breakdowns = append(breakdowns, b.GetProperty())
	}

	parts := []string{head, "kind=insight type=" + insightTypeName(spec.GetInsightType())}
	if len(eventKinds) > 0 {
		parts = append(parts, "events=["+strings.Join(eventKinds, ", ")+"]")
	}
	if len(breakdowns) > 0 {
		parts = append(parts, "breakdowns=["+strings.Join(breakdowns, ", ")+"]")
	}
	return strings.Join(parts, " ")
}

// summarizeDraft gives the model stable tile ids to target update_tile and
// remove_tile, plus enough of each spec to reason about what is already on the
// dashboard — without re-serialising every InsightQuerySpec, which would put
// the turn's input cost back in proportion to dashboard size and undo the main
// reason for choosing an operation-based contract.
func summarizeDraft(draft *dashboardsv1.Dashboard) string {
	if draft == nil {
		return "Dashboard draft: (none)"
	}
	if len(draft.GetTiles()) == 0 {
		return fmt.Sprintf("Dashboard draft %q has no tiles yet.", draft.GetDisplayName())
	}
	lines := make([]string, 0, len(draft.GetTiles())+1)
	lines = append(lines, fmt.Sprintf("Dashboard draft %q (%d tiles):", draft.GetDisplayName(), len(draft.GetTiles())))
	for _, t := range draft.GetTiles() {
		lines = append(lines, describeTile(t))
	}
	return strings.Join(lines, "\n")
}
