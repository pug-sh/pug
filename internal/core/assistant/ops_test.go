package assistant

import (
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	aidashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/ai/dashboards/v1"
)

// tileFromJSON must accept real proto3 JSON — a flat oneof member key
// ("insight"), enum name strings — because that is what the model produces.
func TestTileFromJSON_ReadsFlatOneofMemberAsProto3JSON(t *testing.T) {
	tile, err := tileFromJSON(json.RawMessage(`{
		"displayName": "Weekly actives",
		"insight": {
			"spec": {"insightType": "INSIGHT_TYPE_TRENDS", "events": [{"event": {"kind": "page_view"}}]}
		}
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tile.GetInsight() == nil {
		t.Fatal("content oneof not set to insight")
	}
}

// protojson rejects a shape it cannot decode — an unrecognised key, including
// a {case, value} oneof form, which is not valid proto3 JSON — rather than
// silently dropping it. That makes the parse-failure branch in submit() a
// live, commonly-hit path, not a rare backstop.
func TestTileFromJSON_ErrorsOnUndecodableShape(t *testing.T) {
	if _, err := tileFromJSON(json.RawMessage(`{"content": {"case": "insight", "value": {}}}`)); err == nil {
		t.Fatal("expected error for unknown field")
	}
	if _, err := tileFromJSON(json.RawMessage(`{"totally": "not a tile"}`)); err == nil {
		t.Fatal("expected error for unknown field")
	}
}

func opsTestViolation(message, ruleID string) Violation {
	return Violation{Path: "insight.spec", RuleID: ruleID, Message: message}
}

func opsTestBuildOp(flagged []string) *aidashboardsv1.TileOp {
	return &aidashboardsv1.TileOp{
		Op: &aidashboardsv1.TileOp_Remove{
			Remove: &aidashboardsv1.RemoveTile{TileId: proto.String("t1")},
		},
		Violations: flagged,
	}
}

func TestOnValidationFailure_RetriesWhileBudgetRemains(t *testing.T) {
	out := onValidationFailure("signups", []Violation{opsTestViolation("bad spec", "some.rule")}, 1, opsTestBuildOp)
	if !strings.Contains(out.RetryPrompt, "bad spec") {
		t.Fatalf("retryPrompt = %q", out.RetryPrompt)
	}
	if out.Op != nil || out.Failed != nil {
		t.Fatalf("expected retry-only outcome, got %+v", out)
	}
}

func TestOnValidationFailure_RetryPromptNamesTheField(t *testing.T) {
	out := onValidationFailure("signups", []Violation{opsTestViolation("bad spec", "some.rule")}, 1, opsTestBuildOp)
	if !strings.Contains(out.RetryPrompt, "insight.spec") {
		t.Fatalf("retryPrompt = %q", out.RetryPrompt)
	}
}

func TestOnValidationFailure_EmitsFlaggedOnceBudgetSpent(t *testing.T) {
	msg := "funnel and retention insight types require at least one event"
	out := onValidationFailure("signups", []Violation{opsTestViolation(msg, "some.rule")}, maxRepairAttempts, opsTestBuildOp)
	if out.RetryPrompt != "" {
		t.Fatalf("retryPrompt should be empty, got %q", out.RetryPrompt)
	}
	if out.Op == nil {
		t.Fatal("expected a flagged op")
	}
	if len(out.Op.GetViolations()) != 1 || out.Op.GetViolations()[0] != msg {
		t.Fatalf("violations = %v", out.Op.GetViolations())
	}
}

// The user asked for a tile. Silently producing nothing is the one outcome
// that tells them nothing at all.
func TestOnValidationFailure_NeverDropsATileSilently(t *testing.T) {
	out := onValidationFailure("signups", []Violation{opsTestViolation("bad", "some.rule")}, maxRepairAttempts, opsTestBuildOp)
	if out.Op == nil && out.Failed == nil && out.RetryPrompt == "" {
		t.Fatal("silent drop")
	}
}

// A validator misconfiguration is our bug. Retrying cannot fix it, and
// emitting the tile would ship something that was never actually checked.
func TestOnValidationFailure_ValidatorErrorFailsOutright(t *testing.T) {
	out := onValidationFailure(
		"signups",
		[]Violation{opsTestViolation("validator misconfigured: unresolved attribute", "validator.error")},
		1,
		opsTestBuildOp,
	)
	if out.Op != nil || out.RetryPrompt != "" {
		t.Fatalf("expected outright failure, got %+v", out)
	}
	if out.Failed == nil || !strings.Contains(out.Failed.GetViolations()[0], "validator misconfigured") {
		t.Fatalf("failed = %v", out.Failed)
	}
}
