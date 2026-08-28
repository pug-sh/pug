package assistant

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/grafana/ai-sdk/provider"
	anthropicprovider "github.com/grafana/ai-sdk/providers/anthropic"
	"google.golang.org/protobuf/proto"

	"github.com/pug-sh/pug/internal/core/assistant/assistanttest"
	aidashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/ai/dashboards/v1"
)

func TestRunLoop_EmitsDeltasInOrderAndAccumulatesReply(t *testing.T) {
	model := &assistanttest.ScriptedModel{Scripts: [][]provider.StreamPart{
		assistanttest.TextScript("Hel", "lo."),
	}}

	var got []string
	var trace []ToolCallRecord
	reply, err := runLoop(context.Background(), model, nil, nil, nil, "hi",
		nil, nil, &trace,
		func(delta string) error {
			got = append(got, delta)
			return nil
		})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if reply != "Hello." {
		t.Fatalf("reply = %q", reply)
	}
	if len(got) != 2 || got[0] != "Hel" || got[1] != "lo." {
		t.Fatalf("deltas = %v", got)
	}
	if len(trace) != 0 {
		t.Fatalf("trace = %v", trace)
	}
}

func TestRunLoop_MessageOrderIsSystemSummaryHistoryThenUser(t *testing.T) {
	model := &assistanttest.ScriptedModel{Scripts: [][]provider.StreamPart{
		assistanttest.TextScript("ok"),
	}}
	history := []*aidashboardsv1.Message{
		{Role: aidashboardsv1.Message_ROLE_USER.Enum(), Content: proto.String("earlier question")},
		{Role: aidashboardsv1.Message_ROLE_ASSISTANT.Enum(), Content: proto.String("earlier answer")},
	}

	var trace []ToolCallRecord
	if _, err := runLoop(context.Background(), model, nil, nil, history, "new question",
		nil, nil, &trace, func(string) error { return nil }); err != nil {
		t.Fatalf("err: %v", err)
	}

	prompt := model.Calls[0].Prompt
	wantRoles := []provider.Role{
		provider.RoleSystem,    // system prompt
		provider.RoleUser,      // draft summary
		provider.RoleUser,      // earlier question
		provider.RoleAssistant, // earlier answer
		provider.RoleUser,      // new question
	}
	if len(prompt) != len(wantRoles) {
		t.Fatalf("len(prompt) = %d, want %d", len(prompt), len(wantRoles))
	}
	for i, want := range wantRoles {
		if prompt[i].Role != want {
			t.Fatalf("prompt[%d].Role = %s, want %s", i, prompt[i].Role, want)
		}
	}
	if !strings.Contains(prompt[0].Content[0].Text, "pug analytics dashboard") {
		t.Fatalf("system prompt missing: %q", prompt[0].Content[0].Text)
	}
	if !strings.Contains(prompt[1].Content[0].Text, "Dashboard draft") {
		t.Fatalf("draft summary missing: %q", prompt[1].Content[0].Text)
	}
	if prompt[4].Content[0].Text != "new question" {
		t.Fatalf("last message = %q", prompt[4].Content[0].Text)
	}
}

func TestRunLoop_ToolCallsSettleBeforeReturnAndAreTraced(t *testing.T) {
	args := fmt.Sprintf(`{"intent":"actives","tile":%s}`, validTileJSON)
	model := &assistanttest.ScriptedModel{Scripts: [][]provider.StreamPart{
		assistanttest.ToolCallScript("c1", "add_tile", args),
		assistanttest.TextScript("Added it."),
	}}

	var sink []emittedOp
	opTools := buildOpTools(emptyDraft(), &sink)
	var trace []ToolCallRecord

	reply, err := runLoop(context.Background(), model, nil, nil, nil, "add a tile",
		nil, opTools, &trace, func(string) error { return nil })
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if reply != "Added it." {
		t.Fatalf("reply = %q", reply)
	}
	// The stream has fully completed when runLoop returns, so every tool call
	// has settled and the sink is complete — the caller can drain it knowing
	// nothing more will arrive.
	if len(sink) != 1 || sink[0].op.GetAdd() == nil {
		t.Fatalf("sink = %+v", sink)
	}
	if len(trace) != 1 || trace[0].ToolName != "add_tile" {
		t.Fatalf("trace = %+v", trace)
	}
	if !strings.Contains(string(trace[0].Output), "Accepted.") {
		t.Fatalf("trace output = %s", trace[0].Output)
	}
}

func TestRunLoop_ModelFailureSurfacesAsError(t *testing.T) {
	model := &assistanttest.ScriptedModel{} // zero scripts: first DoStream errors
	var trace []ToolCallRecord
	_, err := runLoop(context.Background(), model, nil, nil, nil, "hi",
		nil, nil, &trace, func(string) error { return nil })
	if err == nil {
		t.Fatal("expected an error from a failing model")
	}
}

func TestRunLoop_EmitFailureStopsForwardingButDrainsStream(t *testing.T) {
	model := &assistanttest.ScriptedModel{Scripts: [][]provider.StreamPart{
		assistanttest.TextScript("a", "b", "c"),
	}}
	var forwarded int
	var trace []ToolCallRecord
	_, err := runLoop(context.Background(), model, nil, nil, nil, "hi",
		nil, nil, &trace, func(string) error {
			forwarded++
			return errors.New("client gone")
		})
	if err == nil || !strings.Contains(err.Error(), "client gone") {
		t.Fatalf("err = %v", err)
	}
	if forwarded != 1 {
		t.Fatalf("forwarded = %d, want 1 (stop after first failure)", forwarded)
	}
}

// The prompt is large and re-sent on every step, so the system block must
// carry a cache breakpoint the anthropic provider recognises.
func TestRunLoop_SystemPromptCarriesCacheBreakpoint(t *testing.T) {
	model := &assistanttest.ScriptedModel{Scripts: [][]provider.StreamPart{
		assistanttest.TextScript("ok"),
	}}

	var trace []ToolCallRecord
	if _, err := runLoop(context.Background(), model, nil, nil, nil, "q",
		nil, nil, &trace, func(string) error { return nil }); err != nil {
		t.Fatalf("err: %v", err)
	}

	system := model.Calls[0].Prompt[0]
	if system.Role != provider.RoleSystem {
		t.Fatalf("prompt[0].Role = %s, want system", system.Role)
	}
	cc, ok := system.ProviderOptions["anthropic"].(anthropicprovider.AnthropicCacheControl)
	if !ok {
		t.Fatalf("system message provider options = %#v, want an anthropic cache control", system.ProviderOptions)
	}
	if cc.CacheType != "ephemeral" {
		t.Fatalf("cache type = %q, want ephemeral", cc.CacheType)
	}
}

// A tools-only turn stores an empty reply; replaying it as an empty text block
// makes the provider reject every later call on the conversation.
func TestRunLoop_EmptyMessagesAreNotReplayed(t *testing.T) {
	model := &assistanttest.ScriptedModel{Scripts: [][]provider.StreamPart{assistanttest.TextScript("ok")}}
	history := []*aidashboardsv1.Message{
		{Role: aidashboardsv1.Message_ROLE_USER.Enum(), Content: proto.String("remove it")},
		{Role: aidashboardsv1.Message_ROLE_ASSISTANT.Enum(), Content: proto.String("")},
	}

	var trace []ToolCallRecord
	if _, err := runLoop(context.Background(), model, nil, nil, history, "next",
		nil, nil, &trace, func(string) error { return nil }); err != nil {
		t.Fatalf("err: %v", err)
	}

	prompt := model.Calls[0].Prompt
	if len(prompt) != 4 { // system, summary, "remove it", "next"
		t.Fatalf("len(prompt) = %d, want 4", len(prompt))
	}
	for _, m := range prompt {
		if m.Role == provider.RoleAssistant {
			t.Fatal("empty assistant message was replayed")
		}
	}
}
