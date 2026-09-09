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
	if err := recordTurnTrace(ctx, rd.Client, testCreds, "conv_1", first); err != nil {
		t.Fatalf("recordTurnTrace: %v", err)
	}
	if err := recordTurnTrace(ctx, rd.Client, testCreds, "conv_1", second); err != nil {
		t.Fatalf("recordTurnTrace: %v", err)
	}

	entries, err := rd.Client.LRange(ctx, traceKey(testCreds, "conv_1"), 0, -1).Result()
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

	ttl, err := rd.Client.TTL(ctx, traceKey(testCreds, "conv_1")).Result()
	if err != nil {
		t.Fatalf("ttl: %v", err)
	}
	if ttl <= 6*24*time.Hour || ttl > 7*24*time.Hour {
		t.Fatalf("ttl = %v, want ~7d", ttl)
	}
}

// The trace is read by hand during debugging: field names stay camelCase
// (projectId, toolCalls, durationMs) and empty collections serialize as [].
func TestRecordTurnTrace_JSONShapeIsStable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	ctx := context.Background()

	trace := testTrace()
	trace.ToolCalls = []ToolCallRecord{}
	if err := recordTurnTrace(ctx, rd.Client, testCreds, "conv_shape", trace); err != nil {
		t.Fatalf("recordTurnTrace: %v", err)
	}
	raw, err := rd.Client.LIndex(ctx, traceKey(testCreds, "conv_shape"), 0).Result()
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

// Traces store the user's prompt and the model's reply, so they need the same
// isolation as history — and the other assertions here derive their key by
// calling traceKey, which would survive a revert.
func TestRecordTurnTrace_IsolatedByProjectAndCustomer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	ctx := context.Background()

	mine := CallerCredentials{JWT: "j", ProjectID: "prj_1", CustomerID: "cus_1"}
	if err := recordTurnTrace(ctx, rd.Client, mine, "shared_id", testTrace()); err != nil {
		t.Fatalf("recordTurnTrace: %v", err)
	}

	for name, theirs := range map[string]CallerCredentials{
		"other customer": {JWT: "j", ProjectID: "prj_1", CustomerID: "cus_2"},
		"other project":  {JWT: "j", ProjectID: "prj_2", CustomerID: "cus_1"},
	} {
		entries, err := rd.Client.LRange(ctx, traceKey(theirs, "shared_id"), 0, -1).Result()
		if err != nil {
			t.Fatalf("%s: lrange: %v", name, err)
		}
		if len(entries) != 0 {
			t.Fatalf("%s: read %d traces from another caller's conversation", name, len(entries))
		}
	}

	// Pinned as a literal so the scope cannot be reverted without a failure.
	if got, want := traceKey(mine, "shared_id"), "debug:prj_1:cus_1:shared_id"; got != want {
		t.Fatalf("traceKey = %q, want %q", got, want)
	}
}
