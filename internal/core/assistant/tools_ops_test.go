package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	aisdk "github.com/grafana/ai-sdk"
	"github.com/grafana/ai-sdk/schema"
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

// Remove is the one destructive op, so a hallucinated id must come back to the
// model rather than reach the client as a confident removal.
func TestRemoveTile_RejectsUnknownID(t *testing.T) {
	var sink []emittedOp
	tools := buildOpTools(draftWithTiles("tile_x"), &sink)

	reply := execOpTool(t, tools["remove_tile"], `{"tileId":"tile_ghost"}`)

	if !strings.HasPrefix(reply, "ERROR:") {
		t.Fatalf("reply = %q, want an ERROR", reply)
	}
	if len(sink) != 0 {
		t.Fatalf("emitted %d ops for a tile the draft does not have", len(sink))
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
	tools := buildOpTools(draftWithTiles("tile_9"), &sink)

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

// draftWithTiles is a draft holding tiles with the given ids and no positions.
func draftWithTiles(ids ...string) *dashboardsv1.Dashboard {
	draft := &dashboardsv1.Dashboard{DisplayName: proto.String("d")}
	for _, id := range ids {
		draft.Tiles = append(draft.Tiles, &dashboardsv1.DashboardTile{Id: proto.String(id)})
	}
	return draft
}

// A remove carries no tile to validate, so the id is the only thing checked.
func TestRemoveTile_EmitsForATileInTheDraft(t *testing.T) {
	var sink []emittedOp
	tools := buildOpTools(draftWithTiles("tile_x"), &sink)

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
	tile.Position = placeBelow(0, nil)
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

// execOpToolAsync mirrors execOpTool for worker goroutines: FailNow must run on
// the test goroutine, so failures are reported with Errorf instead.
func execOpToolAsync(t *testing.T, tool aisdk.Tool, args string) {
	t.Helper()
	if _, err := tool.Execute(context.Background(), json.RawMessage(args), aisdk.ToolExecutionOptions{}); err != nil {
		t.Errorf("Execute returned an error: %v", err)
	}
}

// Mirrors the SDK's per-tool-call goroutines: several op tools from one step
// reaching the shared turn state at once.
func TestOpTools_ConcurrentCallsRaceFreeAndLoseNoOps(t *testing.T) {
	var sink []emittedOp
	tools := buildOpTools(draftWithTiles("tile_0", "tile_1", "tile_2", "tile_3"), &sink)

	const adds, removes = 8, 4
	var wg sync.WaitGroup
	wg.Add(adds + removes)
	for i := range adds {
		go func() {
			defer wg.Done()
			execOpToolAsync(t, tools["add_tile"], addTileArgs(fmt.Sprintf("tile %d", i), validTileJSON))
		}()
	}
	for i := range removes {
		go func() {
			defer wg.Done()
			execOpToolAsync(t, tools["remove_tile"], fmt.Sprintf(`{"tileId":"tile_%d"}`, i))
		}()
	}
	wg.Wait()

	if len(sink) != adds+removes {
		t.Fatalf("sink has %d ops, want %d — concurrent appends dropped some", len(sink), adds+removes)
	}

	// The cursor advances under the same lock, so the placements must not
	// overlap either — the count alone would pass on a torn cursor.
	var placed []*dashboardsv1.GridPosition
	for _, entry := range sink {
		if pos := entry.op.GetAdd().GetTile().GetPosition(); pos != nil {
			placed = append(placed, pos)
		}
	}
	slices.SortFunc(placed, func(a, b *dashboardsv1.GridPosition) int { return int(a.GetY() - b.GetY()) })
	for i := 1; i < len(placed); i++ {
		if placed[i].GetY() < placed[i-1].GetY()+placed[i-1].GetH() {
			t.Fatalf("tiles overlap: y=%d after y=%d h=%d",
				placed[i].GetY(), placed[i-1].GetY(), placed[i-1].GetH())
		}
	}
}

func TestAddTile_TilesInOneTurnDoNotOverlap(t *testing.T) {
	var sink []emittedOp
	// A non-empty draft: the cursor must start below what is already there, not
	// at zero.
	draft := &dashboardsv1.Dashboard{
		DisplayName: proto.String("d"),
		Tiles: []*dashboardsv1.DashboardTile{
			{Id: proto.String("t0"), Position: &dashboardsv1.GridPosition{
				X: proto.Int32(0), Y: proto.Int32(0), W: proto.Int32(36), H: proto.Int32(30),
			}},
		},
	}
	add := buildOpTools(draft, &sink)["add_tile"]

	for i := range 3 {
		execOpTool(t, add, addTileArgs(fmt.Sprintf("tile %d", i), validTileJSON))
	}

	var prevBottom int32 = 30
	for i, entry := range sink {
		pos := entry.op.GetAdd().GetTile().GetPosition()
		if pos.GetY() < prevBottom {
			t.Fatalf("tile %d at y=%d overlaps the tile above it (bottom=%d)", i, pos.GetY(), prevBottom)
		}
		prevBottom = pos.GetY() + pos.GetH()
	}
}

// The cursor does not advance on a failed attempt — otherwise every repair
// round would leave a blank row in the finished dashboard.
func TestAddTile_RepairRetryDoesNotLeaveAGap(t *testing.T) {
	var sink []emittedOp
	add := buildOpTools(emptyDraft(), &sink)["add_tile"]

	execOpTool(t, add, addTileArgs("first", validTileJSON))
	execOpTool(t, add, addTileArgs("second", invalidTileJSON))
	execOpTool(t, add, addTileArgs("second", validTileJSON))

	if len(sink) != 2 {
		t.Fatalf("sink has %d ops, want 2", len(sink))
	}
	first := sink[0].op.GetAdd().GetTile().GetPosition()
	second := sink[1].op.GetAdd().GetTile().GetPosition()
	if got, want := second.GetY(), first.GetY()+first.GetH(); got != want {
		t.Fatalf("second tile at y=%d, want %d — the failed attempt consumed a row", got, want)
	}
}

// The repair budget is keyed on intent, and the SDK enforces InputSchema before
// Execute — so the schema is where an empty intent has to be stopped.
func TestOpToolSchemas_RejectEmptyIntent(t *testing.T) {
	for name, s := range map[string]schema.Schema{
		"add_tile":    addTileInputSchema,
		"update_tile": updateTileInputSchema,
	} {
		if err := s.Validate(json.RawMessage(`{"intent":"","tileId":"t1","tile":{}}`)); err == nil {
			t.Fatalf("%s: empty intent accepted", name)
		}
		if err := s.Validate(json.RawMessage(`{"intent":"actives","tileId":"t1","tile":{}}`)); err != nil {
			t.Fatalf("%s: valid intent rejected: %v", name, err)
		}
	}
}

// An update edits a tile in place — "make this a bar chart" must not move it to
// the bottom of the board.
func TestUpdateTile_KeepsTheExistingPosition(t *testing.T) {
	existing := &dashboardsv1.GridPosition{
		X: proto.Int32(0), Y: proto.Int32(40), W: proto.Int32(36), H: proto.Int32(20),
	}
	draft := &dashboardsv1.Dashboard{
		DisplayName: proto.String("d"),
		Tiles:       []*dashboardsv1.DashboardTile{{Id: proto.String("t1"), Position: existing}},
	}
	var sink []emittedOp
	tools := buildOpTools(draft, &sink)

	execOpTool(t, tools["update_tile"], fmt.Sprintf(`{"intent":"recolour","tileId":"t1","tile":%s}`, validTileJSON))

	got := sink[0].op.GetUpdate().GetTile().GetPosition()
	if got.GetY() != existing.GetY() || got.GetX() != existing.GetX() {
		t.Fatalf("update moved the tile to x=%d y=%d, want x=0 y=40", got.GetX(), got.GetY())
	}

	// And it must not consume a row: a tile added after keeps the draft's bottom.
	execOpTool(t, tools["add_tile"], addTileArgs("new", validTileJSON))
	if y := sink[1].op.GetAdd().GetTile().GetPosition().GetY(); y != 60 {
		t.Fatalf("added tile at y=%d, want 60", y)
	}
}

// Upsert rejects a populated id matching no tile, so emitting the update would
// hand the client an op it could never save.
func TestUpdateTile_RejectsUnknownID(t *testing.T) {
	var sink []emittedOp
	tools := buildOpTools(draftWithTiles("tile_x"), &sink)

	reply := execOpTool(t, tools["update_tile"], fmt.Sprintf(`{"intent":"x","tileId":"missing","tile":%s}`, validTileJSON))

	if !strings.HasPrefix(reply, "ERROR:") {
		t.Fatalf("reply = %q, want an ERROR", reply)
	}
	if len(sink) != 0 {
		t.Fatalf("emitted %d ops for a tile the draft does not have", len(sink))
	}
}

func TestAddTile_DropsModelSuppliedID(t *testing.T) {
	var sink []emittedOp
	tools := buildOpTools(emptyDraft(), &sink)

	withID := strings.Replace(validTileJSON, `"displayName"`, `"id":"abc123","displayName"`, 1)
	if reply := execOpTool(t, tools["add_tile"], addTileArgs("actives", withID)); reply != "Accepted." {
		t.Fatalf("reply = %q", reply)
	}
	if id := sink[0].op.GetAdd().GetTile().GetId(); id != "" {
		t.Fatalf("emitted id = %q, want empty", id)
	}
}

// The budget is per target, not per label: a flagged add must not block a
// later update of an existing tile that happens to reuse the intent.
func TestUpdateTile_NotBlockedByFlaggedAddWithSameIntent(t *testing.T) {
	var sink []emittedOp
	tools := buildOpTools(draftWithTiles("tile_9"), &sink)

	for range maxRepairAttempts {
		execOpTool(t, tools["add_tile"], addTileArgs("signups", invalidTileJSON))
	}
	reply := execOpTool(t, tools["update_tile"],
		fmt.Sprintf(`{"intent":"signups","tileId":"tile_9","tile":%s}`, validTileJSON))
	if reply != "Accepted." {
		t.Fatalf("reply = %q", reply)
	}
}

func TestAddTile_SuccessResetsTheIntentBudget(t *testing.T) {
	var sink []emittedOp
	tools := buildOpTools(emptyDraft(), &sink)

	execOpTool(t, tools["add_tile"], addTileArgs("kpi", invalidTileJSON))
	execOpTool(t, tools["add_tile"], addTileArgs("kpi", validTileJSON))
	// A fresh tile under the same label gets the full budget again.
	reply := execOpTool(t, tools["add_tile"], addTileArgs("kpi", invalidTileJSON))
	if !strings.Contains(reply, "call the tool again") {
		t.Fatalf("reply = %q", reply)
	}
}
