package assistant

import (
	"context"
	"encoding/json"

	"github.com/redis/go-redis/v9"
)

// ToolCallRecord is one tool invocation in a turn's debug trace. Input/Output
// are raw JSON exactly as they crossed the model boundary.
type ToolCallRecord struct {
	ToolName string          `json:"toolName"`
	Input    json.RawMessage `json:"input"`
	Output   json.RawMessage `json:"output"`
}

// FailedIntent mirrors FailedOp for JSON storage (proto messages don't
// json.Marshal cleanly).
type FailedIntent struct {
	Intent     string   `json:"intent"`
	Violations []string `json:"violations"`
}

// TurnTrace deliberately has no credential field — never store the caller's
// JWT here; it's a live credential, not debug data. Field names match the TS
// service's stored shape.
type TurnTrace struct {
	ProjectID  string           `json:"projectId"`
	Message    string           `json:"message"`
	Reply      string           `json:"reply"`
	ToolCalls  []ToolCallRecord `json:"toolCalls"`
	Ops        int              `json:"ops"`
	Failed     []FailedIntent   `json:"failed"`
	Model      string           `json:"model"`
	DurationMs int64            `json:"durationMs"`
}

func traceKey(conversationID string) string {
	return "debug:" + conversationID
}

// recordTurnTrace appends one turn's trace and refreshes the key's TTL.
// Callers treat failures as best-effort: observability must not fail a turn
// that otherwise succeeded.
func recordTurnTrace(ctx context.Context, rdb *redis.Client, conversationID string, trace TurnTrace) error {
	if trace.ToolCalls == nil {
		trace.ToolCalls = []ToolCallRecord{}
	}
	if trace.Failed == nil {
		trace.Failed = []FailedIntent{}
	}
	payload, err := json.Marshal(trace)
	if err != nil {
		return err
	}
	key := traceKey(conversationID)
	if err := rdb.RPush(ctx, key, payload).Err(); err != nil {
		return err
	}
	return rdb.Expire(ctx, key, conversationTTL).Err()
}
