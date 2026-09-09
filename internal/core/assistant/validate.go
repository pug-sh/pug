package assistant

import (
	"errors"
	"fmt"
	"strings"

	protovalidate "buf.build/go/protovalidate"

	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
)

// Violation is one protovalidate rule failure, flattened for the repair loop
// and TileOp.violations. The messages are already human-readable prose taken
// straight from the proto rules, so they serve both as model-facing repair
// input and as user-facing explanation.
type Violation struct {
	Path    string
	RuleID  string
	Message string
}

type ValidateResult struct {
	OK         bool
	Violations []Violation
	Formatted  string
}

func formatViolations(violations []Violation) string {
	lines := make([]string, 0, len(violations))
	for _, v := range violations {
		if v.Path != "" {
			lines = append(lines, fmt.Sprintf("- %s: %s", v.Path, v.Message))
		} else {
			lines = append(lines, "- "+v.Message)
		}
	}
	return strings.Join(lines, "\n")
}

// toResult classifies protovalidate.Validate's error. Split out from
// validateTile so the fail-closed branch is directly testable.
//
// A non-ValidationError (CEL compilation or runtime failure) is a wiring bug
// on our side, not a bad tile. Treating it as "no violations" would fail OPEN —
// the tile would reach the client unvalidated. Surface it as a failure,
// distinctly labelled (ruleID "validator.error") so ops.go can tell it from a
// genuine rule violation and fail the tile outright.
func toResult(err error) ValidateResult {
	if err == nil {
		return ValidateResult{OK: true}
	}

	var verr *protovalidate.ValidationError
	if !errors.As(err, &verr) {
		violations := []Violation{{
			Path:    "",
			RuleID:  "validator.error",
			Message: fmt.Sprintf("validator misconfigured: %v", err),
		}}
		return ValidateResult{OK: false, Violations: violations, Formatted: formatViolations(violations)}
	}

	violations := make([]Violation, 0, len(verr.Violations))
	for _, v := range verr.Violations {
		violations = append(violations, Violation{
			// FieldPathString renders repeated fields as "events[1]"; anything
			// less tells the model nothing about which element to fix.
			Path:    protovalidate.FieldPathString(v.Proto.GetField()),
			RuleID:  v.Proto.GetRuleId(),
			Message: v.Proto.GetMessage(),
		})
	}
	return ValidateResult{OK: false, Violations: violations, Formatted: formatViolations(violations)}
}

// validateTile checks a tile against its own protovalidate rules — the same
// rules DashboardsService.Upsert enforces server-side. Descriptors are compiled
// into the binary, so the CEL enum references (e.g.
// shared.insights.v1.InsightType.INSIGHT_TYPE_FUNNEL) always resolve; there is
// no registry to keep in sync.
func validateTile(tile *dashboardsv1.DashboardTileInput) ValidateResult {
	return toResult(protovalidate.Validate(tile))
}
