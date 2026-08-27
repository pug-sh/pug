// Package assistanttest provides a scripted provider.LanguageModel for testing
// the assistant loop without a real LLM. It lives outside the _test files so
// internal/app/ai's handler tests can drive a real Service with it.
package assistanttest

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/grafana/ai-sdk/provider"
)

// ScriptedModel replays canned provider stream parts, one script per DoStream
// call, and records every call's params for assertions.
type ScriptedModel struct {
	Scripts [][]provider.StreamPart
	Calls   []provider.CallOptions
	// CallDeadlines holds each call's context deadline, zero when it had none.
	CallDeadlines []time.Time
}

var _ provider.LanguageModel = (*ScriptedModel)(nil)

func (m *ScriptedModel) SpecificationVersion() string               { return "v4" }
func (m *ScriptedModel) Provider() string                           { return "scripted" }
func (m *ScriptedModel) ModelID() string                            { return "scripted-model" }
func (m *ScriptedModel) SupportedURLs() map[string][]*regexp.Regexp { return nil }

func (m *ScriptedModel) DoGenerate(context.Context, provider.CallOptions) (*provider.GenerateResult, error) {
	return nil, errors.New("assistanttest: DoGenerate not scripted")
}

func (m *ScriptedModel) DoStream(ctx context.Context, params provider.CallOptions) (*provider.StreamResult, error) {
	call := len(m.Calls)
	m.Calls = append(m.Calls, params)
	deadline, _ := ctx.Deadline()
	m.CallDeadlines = append(m.CallDeadlines, deadline)
	if call >= len(m.Scripts) {
		return nil, errors.New("assistanttest: DoStream called more times than scripted")
	}
	parts := m.Scripts[call]
	ch := make(chan provider.StreamPart, len(parts))
	for _, p := range parts {
		ch <- p
	}
	close(ch)
	return &provider.StreamResult{Stream: ch}, nil
}

// FinishPart ends a step; every script must end with one or the SDK reports
// no output.
func FinishPart(reason string) provider.StreamPart {
	fr := provider.FinishReason{Unified: provider.UnifiedFinishReason(reason)}
	return provider.StreamPart{Type: provider.PartFinish, Usage: &provider.Usage{}, FinishReason: &fr}
}

// TextScript is one assistant step streaming the given deltas, then stopping.
func TextScript(deltas ...string) []provider.StreamPart {
	parts := []provider.StreamPart{{Type: provider.PartTextStart, ID: "t1"}}
	for _, d := range deltas {
		parts = append(parts, provider.StreamPart{Type: provider.PartTextDelta, ID: "t1", Delta: d})
	}
	parts = append(parts, provider.StreamPart{Type: provider.PartTextEnd, ID: "t1"}, FinishPart("stop"))
	return parts
}

// ToolCallScript is one step that calls a single tool. Input is stringified
// JSON — that is the provider wire shape, not a mistake.
func ToolCallScript(callID, toolName, inputJSON string) []provider.StreamPart {
	return []provider.StreamPart{
		{Type: provider.PartToolCall, ToolCallID: callID, ToolName: toolName, Input: inputJSON},
		FinishPart("tool-calls"),
	}
}
