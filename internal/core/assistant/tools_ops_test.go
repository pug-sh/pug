package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	aisdk "github.com/grafana/ai-sdk"
	"google.golang.org/protobuf/proto"

	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
)

// Flat oneof member key ("insight"), enum name strings — real proto3 JSON, the
// shape tileFromJSON actually accepts and what the model produces.
const validTileJSON = `{
	"displayName": "Weekly actives",
	"insight": {"spec": {"insightType": "INSIGHT_TYPE_TRENDS", "events": [{"event": {"kind": "page_view"}}]}}
}`

const invalidTileJSON = `{
	"displayName": "Broken funnel",
	"insight": {"spec": {"insightType": "INSIGHT_TYPE_FUNNEL", "events": []}}
}`

func emptyDraft() *dashboardsv1.Dashboard {
	return &dashboardsv1.Dashboard{DisplayName: proto.String("d")}
}

func execOpTool(t *testing.T, tool aisdk.Tool, args string) string {
	t.Helper()
	out, err := tool.Execute(context.Background(), json.RawMessage(args), aisdk.ToolExecutionOptions{})
	if err != nil {
		t.Fatalf("Execute returned an error — op tools must reply with strings: %v", err)
	}
	var s string
	if err := json.Unmarshal(out, &s); err != nil {
		t.Fatalf("tool output is not a JSON string: %v (%s)", err, out)
	}
	return s
}

func addTileArgs(intent, tileJSON string) string {
	return fmt.Sprintf(`{"intent":%q,"tile":%s}`, intent, tileJSON)
}

func TestAddTile_ValidTileAcceptedAndEmittedUnflagged(t *testing.T) {
	var sink []emittedOp
	tools := buildOpTools(emptyDraft(), &sink)

	reply := execOpTool(t, tools["add_tile"], addTileArgs("actives", validTileJSON))

	if reply != "Accepted." {
		t.Fatalf("reply = %q", reply)
	}
	if len(sink) != 1 || sink[0].op == nil {
		t.Fatalf("sink = %+v", sink)
	}
	if sink[0].op.GetAdd() == nil {
		t.Fatal("expected an add op")
	}
	if len(sink[0].op.GetViolations()) != 0 {
		t.Fatalf("violations = %v", sink[0].op.GetViolations())
	}
}

func TestAddTile_AssignsCompleteGridPosition(t *testing.T) {
	var sink []emittedOp
	tools := buildOpTools(emptyDraft(), &sink)
	execOpTool(t, tools["add_tile"], addTileArgs("actives", validTileJSON))

	pos := sink[0].op.GetAdd().GetTile().GetPosition()
	if pos == nil {
		t.Fatal("no position assigned")
	}
	if pos.GetX() != 0 || pos.GetY() != 0 || pos.GetW() <= 0 || pos.GetH() <= 0 {
		t.Fatalf("pos = %+v", pos)
	}
}

func TestAddTile_InvalidTileHandedBackForRepairNotEmitted(t *testing.T) {
	var sink []emittedOp
	tools := buildOpTools(emptyDraft(), &sink)

	reply := execOpTool(t, tools["add_tile"], addTileArgs("funnel", invalidTileJSON))

	if !strings.Contains(reply, "funnel and retention insight types require at least one event") {
		t.Fatalf("reply = %q", reply)
	}
	if len(sink) != 0 {
		t.Fatalf("sink should be empty, got %+v", sink)
	}
}

// THE invariant. Flagged-emit means an invalid tile does reach the client —
// but never without violations attached, because that field is the only thing
// stopping the webapp saving it into a request Upsert would reject.
func TestAddTile_NeverEmittedUnflaggedEvenAfterBudgetSpent(t *testing.T) {
	var sink []emittedOp
	tools := buildOpTools(emptyDraft(), &sink)

	for range maxRepairAttempts {
		execOpTool(t, tools["add_tile"], addTileArgs("funnel", invalidTileJSON))
	}

	if len(sink) != 1 {
		t.Fatalf("len(sink) = %d, want 1", len(sink))
	}
	op := sink[0].op
	if op == nil {
		t.Fatal("expected a flagged op")
	}
	if len(op.GetViolations()) == 0 {
		t.Fatal("flagged op without violations")
	}
	if !strings.Contains(op.GetViolations()[0], "at least one event") {
		t.Fatalf("violations = %v", op.GetViolations())
	}
}

func TestAddTile_NoSecondFlaggedOpAfterBudgetSpent(t *testing.T) {
	var sink []emittedOp
	tools := buildOpTools(emptyDraft(), &sink)

	for range maxRepairAttempts {
		execOpTool(t, tools["add_tile"], addTileArgs("funnel", invalidTileJSON))
	}
	reply := execOpTool(t, tools["add_tile"], addTileArgs("funnel", invalidTileJSON))

	if len(sink) != 1 {
		t.Fatalf("len(sink) = %d, want 1", len(sink))
	}
	if strings.Contains(reply, "Emitted with") {
		t.Fatalf("reply should be the given-up notice, got %q", reply)
	}
	if !strings.Contains(reply, "already flagged for manual correction") {
		t.Fatalf("reply = %q", reply)
	}
}

func TestAddTile_PerIntentBudgetsDoNotConsumeEachOther(t *testing.T) {
	var sink []emittedOp
	tools := buildOpTools(emptyDraft(), &sink)

	execOpTool(t, tools["add_tile"], addTileArgs("funnel-a", invalidTileJSON))
	execOpTool(t, tools["add_tile"], addTileArgs("funnel-b", invalidTileJSON))

	// Two different tiles, one attempt each — neither should have given up yet.
	if len(sink) != 0 {
		t.Fatalf("sink = %+v", sink)
	}
}

// protojson errors on a shape it cannot decode — an unrecognised key,
// including a {case, value} oneof form — rather than silently dropping it.
// That makes the parse branch in submit() a live, commonly-hit path.
func TestAddTile_UnparseableGetsRetryPromptWhileBudgetRemains(t *testing.T) {
	var sink []emittedOp
	tools := buildOpTools(emptyDraft(), &sink)

	reply := execOpTool(t, tools["add_tile"],
		`{"intent":"nonsense","tile":{"content":{"case":"not_a_real_case","value":{}}}}`)

	if !strings.Contains(reply, "could not be parsed") {
		t.Fatalf("reply = %q", reply)
	}
	if len(sink) != 0 {
		t.Fatalf("sink = %+v", sink)
	}
}

func TestAddTile_UnparseablePastBudgetReportedFailedNotEmitted(t *testing.T) {
	var sink []emittedOp
	tools := buildOpTools(emptyDraft(), &sink)

	for range maxRepairAttempts {
		execOpTool(t, tools["add_tile"], `{"intent":"nonsense","tile":{"totally":"not a tile"}}`)
	}

	if len(sink) != 1 {
		t.Fatalf("len(sink) = %d", len(sink))
	}
	if sink[0].failed == nil || sink[0].op != nil {
		t.Fatalf("expected failed-only entry, got %+v", sink[0])
	}
	if !strings.Contains(sink[0].failed.GetViolations()[0], "malformed tile") {
		t.Fatalf("violations = %v", sink[0].failed.GetViolations())
	}
}

// Parse failures and validation failures share one per-intent budget, so a
// model alternating between the two failure modes cannot retry forever.
func TestAddTile_ParseAndValidationFailuresShareTheBudget(t *testing.T) {
	var sink []emittedOp
	tools := buildOpTools(emptyDraft(), &sink)

	execOpTool(t, tools["add_tile"], `{"intent":"mixed","tile":{"totally":"not a tile"}}`)
	execOpTool(t, tools["add_tile"], addTileArgs("mixed", invalidTileJSON))
	execOpTool(t, tools["add_tile"], addTileArgs("mixed", invalidTileJSON))

	// Third attempt overall hit the budget: flagged emission, not a retry.
	if len(sink) != 1 || sink[0].op == nil {
		t.Fatalf("sink = %+v", sink)
	}
}

func TestUpdateTile_WrapsTileIdAndReplacementTile(t *testing.T) {
	var sink []emittedOp
	tools := buildOpTools(emptyDraft(), &sink)

	reply := execOpTool(t, tools["update_tile"],
		fmt.Sprintf(`{"intent":"rename","tileId":"tile_9","tile":%s}`, validTileJSON))

	if reply != "Accepted." {
		t.Fatalf("reply = %q", reply)
	}
	update := sink[0].op.GetUpdate()
	if update == nil || update.GetTileId() != "tile_9" {
		t.Fatalf("op = %+v", sink[0].op)
	}
}

func TestRemoveTile_EmitsWithoutValidation(t *testing.T) {
	var sink []emittedOp
	tools := buildOpTools(emptyDraft(), &sink)

	reply := execOpTool(t, tools["remove_tile"], `{"tileId":"tile_x"}`)

	if reply != "Removed." {
		t.Fatalf("reply = %q", reply)
	}
	remove := sink[0].op.GetRemove()
	if remove == nil || remove.GetTileId() != "tile_x" {
		t.Fatalf("op = %+v", sink[0].op)
	}
	if len(sink[0].op.GetViolations()) != 0 {
		t.Fatalf("violations = %v", sink[0].op.GetViolations())
	}
}

// Guards against the worked example silently going stale: if a future proto
// change makes this shape invalid, this test fails loudly instead of the tool
// description quietly telling the model to produce a broken tile.
func TestExampleTileJSON_IsItselfValid(t *testing.T) {
	tile, err := tileFromJSON(json.RawMessage(exampleTileJSON()))
	if err != nil {
		t.Fatalf("example does not parse: %v", err)
	}
	tile.Position = placeTile(nil, nil)
	if result := validateTile(tile); !result.OK {
		t.Fatalf("example does not validate: %s", result.Formatted)
	}
}

func TestExampleTileJSON_EmbeddedInToolDescriptions(t *testing.T) {
	var sink []emittedOp
	tools := buildOpTools(emptyDraft(), &sink)
	for _, name := range []string{"add_tile", "update_tile"} {
		if !strings.Contains(tools[name].Description, exampleTileJSON()) {
			t.Fatalf("%s description missing the worked example", name)
		}
	}
}
