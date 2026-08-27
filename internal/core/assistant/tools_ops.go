package assistant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	aisdk "github.com/grafana/ai-sdk"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	aidashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/ai/dashboards/v1"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
)

// emittedOp is one sink entry: either an op to stream to the client or a
// failed intent for TurnDone.failed. Exactly one field is set.
type emittedOp struct {
	op     *aidashboardsv1.TileOp
	failed *aidashboardsv1.FailedOp
}

// exampleTileJSON is the worked example embedded in the add_tile/update_tile
// descriptions. Rendered through the real generated types and protojson, not
// hand-typed — a proto rename breaks the build here instead of leaving a stale
// example in a prompt string. Genuine proto3 JSON: a flat oneof member key
// ("insight"), not a {case, value} shape. Compacted because protojson output
// whitespace is deliberately non-deterministic. Covered by a self-check test:
// this string must itself pass validateTile.
var exampleTileJSON = sync.OnceValue(func() string {
	tile := &dashboardsv1.DashboardTileInput{
		DisplayName: proto.String("Weekly active users by country"),
		Content: &dashboardsv1.DashboardTileInput_Insight{Insight: &dashboardsv1.InsightTileContent{
			Spec: &insightsv1.InsightQuerySpec{
				InsightType: insightsv1.InsightType_INSIGHT_TYPE_TRENDS.Enum(),
				Events: []*insightsv1.EventQuery{
					{Event: &commonv1.EventFilter{Kind: proto.String("signin")}},
				},
				Breakdowns: []*insightsv1.Breakdown{{Property: proto.String("$country")}},
			},
		}},
	}
	raw, err := protojson.Marshal(tile)
	if err != nil {
		// Unreachable for a well-formed generated message; the self-check test
		// pins validity of the rendered example.
		panic(fmt.Sprintf("assistant: example tile does not marshal: %v", err))
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		panic(fmt.Sprintf("assistant: example tile does not compact: %v", err))
	}
	return compact.String()
})

// An empty intent would collapse every unlabelled tile into one shared repair
// budget. The SDK validates InputSchema before Execute, so this is enforced.
var addTileInputSchema = mustSchema(`{
	"type": "object",
	"properties": {
		"intent": {"type": "string", "minLength": 1, "description": "One short phrase for what this tile shows."},
		"tile": {"type": "object"}
	},
	"required": ["intent", "tile"]
}`)

var updateTileInputSchema = mustSchema(`{
	"type": "object",
	"properties": {
		"intent": {"type": "string", "minLength": 1},
		"tileId": {"type": "string", "minLength": 1},
		"tile": {"type": "object"}
	},
	"required": ["intent", "tileId", "tile"]
}`)

var removeTileInputSchema = mustSchema(`{
	"type": "object",
	"properties": {
		"tileId": {"type": "string", "minLength": 1}
	},
	"required": ["tileId"]
}`)

// buildOpTools builds the emit tools for one turn, closed over the draft and
// the sink. A tool's return value is what the model reads next, so handing
// back the protovalidate violations and letting it call again IS the repair
// loop — there is no separate retry machinery.
//
// The SDK runs each of a step's tool calls on its own goroutine, so `mu` guards
// every piece of turn state: a map race here is a fatal runtime error no
// recover catches.
func buildOpTools(draft *dashboardsv1.Dashboard, sink *[]emittedOp) aisdk.ToolSet {
	var mu sync.Mutex
	// Attempts are counted per intent, so one struggling tile cannot consume
	// another's budget.
	attempts := map[string]int{}
	// Intents already resolved to a terminal outcome (flagged-emitted or
	// failed). Without this, a model that keeps retrying past the repair budget
	// gets a fresh emission on every single call — onValidationFailure returns
	// an op for every attempt >= maxRepairAttempts, not just the first one over
	// the line.
	givenUp := map[string]bool{}
	// The draft never gains the tiles this turn places, so placement tracks its
	// own cursor or they all land on one row.
	bottom := draftBottom(draft)

	// keep is the position an update must preserve; nil places below the cursor.
	submit := func(intent string, rawTile json.RawMessage, keep *dashboardsv1.GridPosition, wrap func(tile *dashboardsv1.DashboardTileInput, flagged []string) *aidashboardsv1.TileOp) string {
		mu.Lock()
		defer mu.Unlock()

		if givenUp[intent] {
			return fmt.Sprintf(
				"%q was already flagged for manual correction after %d failed attempts. Do not call add_tile or update_tile again for this intent.",
				intent, maxRepairAttempts)
		}

		// Shared across the parse and validation paths, so a model that keeps
		// sending unparseable JSON exhausts the same budget as one that sends
		// parseable-but-invalid tiles, instead of retrying forever.
		attempts[intent]++
		attempt := attempts[intent]

		tile, err := tileFromJSON(rawTile)
		if err != nil {
			// Real proto3 JSON that protojson still can't decode: unknown keys,
			// a wrong enum name, wrong types. Same repair budget as a validation
			// failure — a model can often fix this kind of mistake once shown it.
			if attempt < maxRepairAttempts {
				return fmt.Sprintf("That tile could not be parsed: %v\nFix the shape and call the tool again.", err)
			}
			givenUp[intent] = true
			*sink = append(*sink, emittedOp{failed: &aidashboardsv1.FailedOp{
				Intent:     proto.String(intent),
				Violations: []string{fmt.Sprintf("malformed tile: %v", err)},
			}})
			return fmt.Sprintf("Could not build that tile: %v", err)
		}
		// An update keeps where it already is — "make this a bar chart" must not
		// move the tile. Otherwise the model's proposed position is a hint: w/h
		// honoured (clamped), x/y ignored.
		advance := func() {}
		if keep != nil {
			tile.Position = keep
		} else {
			tile.Position = placeBelow(bottom, tile.GetPosition())
			// Only once the tile is emitted, so a repair retry re-places it
			// rather than burning a row.
			advance = func() { bottom = tile.GetPosition().GetY() + tile.GetPosition().GetH() }
		}

		result := validateTile(tile)
		if result.OK {
			advance()
			*sink = append(*sink, emittedOp{op: wrap(tile, nil)})
			return "Accepted."
		}

		outcome := onValidationFailure(intent, result.Violations, attempt, func(flagged []string) *aidashboardsv1.TileOp {
			return wrap(tile, flagged)
		})

		if outcome.RetryPrompt != "" {
			return outcome.RetryPrompt
		}
		if outcome.Failed != nil {
			givenUp[intent] = true
			*sink = append(*sink, emittedOp{failed: outcome.Failed})
			return "Could not build that tile: " + strings.Join(outcome.Failed.GetViolations(), "; ")
		}
		givenUp[intent] = true
		advance()
		*sink = append(*sink, emittedOp{op: outcome.Op})
		return fmt.Sprintf(
			"Emitted with %d failed validation attempts. The user will be asked to correct it by hand — tell them briefly what is wrong.",
			maxRepairAttempts)
	}

	return aisdk.ToolSet{
		"add_tile": {
			Description: "Add a new tile to the dashboard. Provide a DashboardTileInput as JSON. If the tile is " +
				"invalid you will get the exact validation errors back — fix them and call again. Grid " +
				"position is assigned automatically; you may suggest w and h.\n\n" +
				"Worked example — a real, valid tile:\n" + exampleTileJSON(),
			InputSchema: addTileInputSchema,
			Execute: func(_ context.Context, input json.RawMessage, _ aisdk.ToolExecutionOptions) (json.RawMessage, error) {
				var args struct {
					Intent string          `json:"intent"`
					Tile   json.RawMessage `json:"tile"`
				}
				if err := json.Unmarshal(input, &args); err != nil {
					return jsonString("ERROR: invalid tool input: " + err.Error())
				}
				return jsonString(submit(args.Intent, args.Tile, nil, func(tile *dashboardsv1.DashboardTileInput, flagged []string) *aidashboardsv1.TileOp {
					return &aidashboardsv1.TileOp{
						Op:         &aidashboardsv1.TileOp_Add{Add: &aidashboardsv1.AddTile{Tile: tile}},
						Violations: flagged,
					}
				}))
			},
		},

		"update_tile": {
			Description: "Replace an existing tile. Provide the tile id and the FULL updated DashboardTileInput — " +
				"this is a replacement, not a patch, so include every field the tile should keep.\n\n" +
				"Worked example of the tile shape (id/position differ per call):\n" + exampleTileJSON(),
			InputSchema: updateTileInputSchema,
			Execute: func(_ context.Context, input json.RawMessage, _ aisdk.ToolExecutionOptions) (json.RawMessage, error) {
				var args struct {
					Intent string          `json:"intent"`
					TileID string          `json:"tileId"`
					Tile   json.RawMessage `json:"tile"`
				}
				if err := json.Unmarshal(input, &args); err != nil {
					return jsonString("ERROR: invalid tool input: " + err.Error())
				}
				return jsonString(submit(args.Intent, args.Tile, existingPosition(draft, args.TileID), func(tile *dashboardsv1.DashboardTileInput, flagged []string) *aidashboardsv1.TileOp {
					return &aidashboardsv1.TileOp{
						Op: &aidashboardsv1.TileOp_Update{Update: &aidashboardsv1.UpdateTile{
							TileId: proto.String(args.TileID),
							Tile:   tile,
						}},
						Violations: flagged,
					}
				}))
			},
		},

		"remove_tile": {
			Description: "Remove a tile from the dashboard by id. Only call this when the user asks for something " +
				"to be removed — tiles you simply do not mention are left alone.",
			InputSchema: removeTileInputSchema,
			Execute: func(_ context.Context, input json.RawMessage, _ aisdk.ToolExecutionOptions) (json.RawMessage, error) {
				var args struct {
					TileID string `json:"tileId"`
				}
				if err := json.Unmarshal(input, &args); err != nil {
					return jsonString("ERROR: invalid tool input: " + err.Error())
				}
				// The only destructive op, so it does not take a hallucinated id on
				// trust. A tile added earlier this turn has no id until Upsert
				// assigns one, so the draft is the whole universe of removable ids.
				if !draftHasTile(draft, args.TileID) {
					return jsonString(fmt.Sprintf(
						"ERROR: the draft has no tile with id %q. Do not remove a tile you cannot see.", args.TileID))
				}
				appendRemove := func() {
					mu.Lock()
					defer mu.Unlock()
					*sink = append(*sink, emittedOp{op: &aidashboardsv1.TileOp{
						Op: &aidashboardsv1.TileOp_Remove{Remove: &aidashboardsv1.RemoveTile{
							TileId: proto.String(args.TileID),
						}},
					}})
				}
				appendRemove()
				return jsonString("Removed.")
			},
		},
	}
}
