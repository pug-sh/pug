package assistant

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pug-sh/pug/internal/testutil"
)

func testTrace() TurnTrace {
	return TurnTrace{
		ProjectID: "prj_1",
		Message:   "show weekly actives",
		Reply:     "Added a tile.",
		ToolCalls: []ToolCallRecord{{
			ToolName: "get_insights_filter_schema",
			Input:    json.RawMessage(`{"eventKind":""}`),
			Output:   json.RawMessage(`"{\"events\":[]}"`),
		}},
		Ops:        1,
		Failed:     []FailedIntent{},
		Model:      "agent=anthropic:claude-opus-5",
		DurationMs: 842,
	}
}

func TestRecordTurnTrace_PushesInOrderAndSetsTTL(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	ctx := context.Background()

	first := testTrace()
	second := testTrace()
	second.Message = "second turn"
	if err := recordTurnTrace(ctx, rd.Client, "conv_1", first); err != nil {
		t.Fatalf("recordTurnTrace: %v", err)
	}
	if err := recordTurnTrace(ctx, rd.Client, "conv_1", second); err != nil {
		t.Fatalf("recordTurnTrace: %v", err)
	}

	entries, err := rd.Client.LRange(ctx, "debug:conv_1", 0, -1).Result()
	if err != nil {
		t.Fatalf("lrange: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len = %d", len(entries))
	}
	var got TurnTrace
	if err := json.Unmarshal([]byte(entries[1]), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Message != "second turn" || got.Ops != 1 || got.Model != "agent=anthropic:claude-opus-5" {
		t.Fatalf("got %+v", got)
	}

	ttl, err := rd.Client.TTL(ctx, "debug:conv_1").Result()
	if err != nil {
		t.Fatalf("ttl: %v", err)
	}
	if ttl <= 6*24*time.Hour || ttl > 7*24*time.Hour {
		t.Fatalf("ttl = %v, want ~7d", ttl)
	}
}

// TS-shape parity: field names are camelCase (projectId, toolCalls, durationMs)
// and empty collections serialize as [] not null.
func TestRecordTurnTrace_JSONShapeIsTSCompatible(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	ctx := context.Background()

	trace := testTrace()
	trace.ToolCalls = []ToolCallRecord{}
	if err := recordTurnTrace(ctx, rd.Client, "conv_shape", trace); err != nil {
		t.Fatalf("recordTurnTrace: %v", err)
	}
	raw, err := rd.Client.LIndex(ctx, "debug:conv_shape", 0).Result()
	if err != nil {
		t.Fatalf("lindex: %v", err)
	}
	for _, want := range []string{`"projectId":"prj_1"`, `"toolCalls":[]`, `"failed":[]`, `"durationMs":842`} {
		if !strings.Contains(raw, want) {
			t.Fatalf("trace JSON missing %s:\n%s", want, raw)
		}
	}
	if strings.Contains(raw, "jwt") {
		t.Fatalf("trace must never carry a credential field: %s", raw)
	}
}
