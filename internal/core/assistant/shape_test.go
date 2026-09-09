package assistant

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/grafana/ai-sdk/schema"
)

// The schema we hand the model and the tile we accept must agree — otherwise
// the prompt is teaching a shape tileFromJSON rejects.
func TestTileShapeAcceptsWorkedExample(t *testing.T) {
	s, err := schema.SchemaFromJSON(json.RawMessage(tileShapeJSON()))
	if err != nil {
		t.Fatalf("rendered shape is not a valid schema: %v", err)
	}
	if err := s.Validate(json.RawMessage(exampleTileJSON())); err != nil {
		t.Fatalf("worked example does not match the shape we advertise: %v", err)
	}
}

func TestTileShapeOmitsServerAssignedFields(t *testing.T) {
	var root map[string]any
	if err := json.Unmarshal([]byte(tileShapeJSON()), &root); err != nil {
		t.Fatal(err)
	}
	props, ok := root["properties"].(map[string]any)
	if !ok {
		t.Fatal("shape has no properties")
	}
	for _, k := range []string{"id", "position", "content"} {
		if _, found := props[k]; found {
			t.Errorf("shape exposes %q, which submit() assigns", k)
		}
	}
	if _, found := props["insight"]; !found {
		t.Error("shape has no flat insight key")
	}
	if strings.Contains(tileShapeJSON(), `"which"`) {
		t.Error("shape still carries the MCP oneof discriminator, which protojson rejects")
	}
}

func TestTileRulesCoverCrossFieldConstraints(t *testing.T) {
	rules := tileRules()
	// A representative rule from each level reachable from a tile: the spec
	// itself, a nested spec message, and a filter.
	for _, want := range []string{
		"conversion_window is only valid for funnel insight type",
		"top k insight type requires top_k",
		"between/not_between operators require exactly 2 values",
	} {
		if !strings.Contains(rules, want) {
			t.Errorf("rules omit %q", want)
		}
	}
	seen := map[string]bool{}
	for line := range strings.SplitSeq(rules, "\n") {
		if seen[line] {
			t.Errorf("duplicate rule %q", line)
		}
		seen[line] = true
	}
}

func TestSystemPromptCarriesShapeAndRules(t *testing.T) {
	p := buildSystemPrompt()
	if !strings.Contains(p, tileShapeJSON()) {
		t.Error("system prompt does not carry the tile shape")
	}
	if !strings.Contains(p, tileRules()) {
		t.Error("system prompt does not carry the tile rules")
	}
}
