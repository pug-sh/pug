package assistant

import (
	"encoding/json"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	aidashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/ai/dashboards/v1"
	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
)

// maxRepairAttempts is how many times a single tile may be handed back for
// repair before giving up.
const maxRepairAttempts = 3

// AttemptOutcome is the decision for one failed validation attempt. Exactly
// one field is set.
type AttemptOutcome struct {
	// Op: emit this (flagged) op to the client.
	Op *aidashboardsv1.TileOp
	// Failed: give up; the turn reports this in TurnDone.failed.
	Failed *aidashboardsv1.FailedOp
	// RetryPrompt: hand this text back to the model and let it try again.
	RetryPrompt string
}

// tileFromJSON decodes the model's tile JSON via protojson, not proto struct
// literals: the model produces real proto3 JSON (a flat "insight" or
// "markdown" key for the content oneof, canonical enum name strings).
// protojson rejects unknown fields — including a {case, value} oneof shape —
// with an error, which feeds the parse-failure repair path in submit().
func tileFromJSON(raw json.RawMessage) (*dashboardsv1.DashboardTileInput, error) {
	tile := &dashboardsv1.DashboardTileInput{}
	if err := protojson.Unmarshal(raw, tile); err != nil {
		return nil, err
	}
	return tile, nil
}

// onValidationFailure decides what happens when a tile still fails validation
// after `attempt` repair attempts (1-indexed).
//
// Policy: retry while budget remains, then emit the tile FLAGGED rather than
// dropping it. The user asked for a tile; a silent no-op is the one outcome
// that tells them nothing. The webapp renders a flagged tile as a must-fix
// draft and blocks save — Upsert enforces the same rules server-side, so a
// flagged tile can never be persisted by accident.
func onValidationFailure(
	intent string,
	violations []Violation,
	attempt int,
	buildOp func(flagged []string) *aidashboardsv1.TileOp,
) AttemptOutcome {
	// A validator misconfiguration is our bug, not the model's. Retrying cannot
	// fix it and emitting the tile would ship something never actually checked,
	// so fail the tile outright and let the error reach an operator.
	for _, v := range violations {
		if v.RuleID == "validator.error" {
			messages := make([]string, 0, len(violations))
			for _, vv := range violations {
				messages = append(messages, vv.Message)
			}
			return AttemptOutcome{Failed: &aidashboardsv1.FailedOp{
				Intent:     proto.String(intent),
				Violations: messages,
			}}
		}
	}

	if attempt < maxRepairAttempts {
		return AttemptOutcome{
			RetryPrompt: "That tile is not valid. Fix these and call the tool again:\n" + formatViolations(violations),
		}
	}

	// Budget spent: emit the tile flagged rather than dropping it. The
	// violation text is readable enough to point the user at what to fix, and
	// TileOp.violations is what stops the webapp saving it.
	messages := make([]string, 0, len(violations))
	for _, v := range violations {
		messages = append(messages, v.Message)
	}
	return AttemptOutcome{Op: buildOp(messages)}
}
