package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/grafana/ai-sdk/provider"
	"github.com/redis/go-redis/v9"

	"github.com/pug-sh/pug/internal/core/assistant/assistanttest"
	aidashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/ai/dashboards/v1"
	"github.com/pug-sh/pug/internal/testutil"
)

var serviceTestCreds = CallerCredentials{JWT: "test-jwt-value", ProjectID: "prj_1"}

func newTestService(rdb *redis.Client, scripts [][]provider.StreamPart) (*Service, *assistanttest.ScriptedModel) {
	model := &assistanttest.ScriptedModel{Scripts: scripts}
	svc := NewService(rdb, stubInsightsClient(), model, nil, "agent=test:scripted")
	return svc, model
}

func collectTurn(t *testing.T, svc *Service, conversationID, message string) []*aidashboardsv1.TurnResponse {
	t.Helper()
	var out []*aidashboardsv1.TurnResponse
	err := svc.Turn(context.Background(), conversationID, nil, message, serviceTestCreds,
		func(chunk *aidashboardsv1.TurnResponse) error {
			out = append(out, chunk)
			return nil
		})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	return out
}

func TestServiceTurn_StreamsTextThenOpsThenDone(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	svc, _ := newTestService(rd.Client, [][]provider.StreamPart{
		assistanttest.ToolCallScript("c1", "add_tile", fmt.Sprintf(`{"intent":"actives","tile":%s}`, validTileJSON)),
		assistanttest.TextScript("Added it."),
	})

	chunks := collectTurn(t, svc, "conv_svc1", "add a tile")

	var kinds []string
	var text strings.Builder
	for _, c := range chunks {
		switch {
		case c.GetText() != "":
			kinds = append(kinds, "text")
			text.WriteString(c.GetText())
		case c.GetOp() != nil:
			kinds = append(kinds, "op")
		case c.GetDone() != nil:
			kinds = append(kinds, "done")
		}
	}
	// Deterministic chunk order: all text, then all ops, then done.
	if want := []string{"text", "op", "done"}; strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("chunk order = %v", kinds)
	}
	if text.String() != "Added it." {
		t.Fatalf("text = %q", text.String())
	}
	last := chunks[len(chunks)-1]
	if len(last.GetDone().GetFailed()) != 0 {
		t.Fatalf("failed = %v", last.GetDone().GetFailed())
	}
}

func TestServiceTurn_PersistsHistoryAndServesItNextTurn(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	svc, model := newTestService(rd.Client, [][]provider.StreamPart{
		assistanttest.TextScript("First reply."),
		assistanttest.TextScript("Second reply."),
	})

	collectTurn(t, svc, "conv_hist", "first message")
	collectTurn(t, svc, "conv_hist", "second message")

	// Second call's prompt: system, draft summary, then the persisted first
	// exchange, then the new message.
	prompt := model.Calls[1].Prompt
	if len(prompt) != 5 {
		t.Fatalf("len(prompt) = %d, want 5", len(prompt))
	}
	if prompt[2].Content[0].Text != "first message" || prompt[2].Role != provider.RoleUser {
		t.Fatalf("prompt[2] = %+v", prompt[2])
	}
	if prompt[3].Content[0].Text != "First reply." || prompt[3].Role != provider.RoleAssistant {
		t.Fatalf("prompt[3] = %+v", prompt[3])
	}

	// And the stored history has TTL.
	ttl, err := rd.Client.TTL(context.Background(), "conversation:conv_hist:messages").Result()
	if err != nil || ttl <= 6*24*time.Hour {
		t.Fatalf("ttl = %v err = %v", ttl, err)
	}
}

func TestServiceTurn_RecordsTraceWithoutCredential(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	svc, _ := newTestService(rd.Client, [][]provider.StreamPart{
		assistanttest.ToolCallScript("c1", "add_tile", fmt.Sprintf(`{"intent":"actives","tile":%s}`, validTileJSON)),
		assistanttest.TextScript("Added it."),
	})

	collectTurn(t, svc, "conv_trace", "add a tile")

	raw, err := rd.Client.LIndex(context.Background(), "debug:conv_trace", 0).Result()
	if err != nil {
		t.Fatalf("lindex: %v", err)
	}
	var trace TurnTrace
	if err := json.Unmarshal([]byte(raw), &trace); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if trace.ProjectID != "prj_1" || trace.Ops != 1 || trace.Model != "agent=test:scripted" || trace.Reply != "Added it." {
		t.Fatalf("trace = %+v", trace)
	}
	if len(trace.ToolCalls) != 1 || trace.ToolCalls[0].ToolName != "add_tile" {
		t.Fatalf("toolCalls = %+v", trace.ToolCalls)
	}
	// The caller's JWT is a live credential and must never be stored.
	if strings.Contains(raw, serviceTestCreds.JWT) {
		t.Fatalf("trace leaked the JWT: %s", raw)
	}
}

func TestServiceTurn_AbandonedIntentReachesDoneFailed(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	badArgs := `{"intent":"nonsense","tile":{"totally":"not a tile"}}`
	svc, _ := newTestService(rd.Client, [][]provider.StreamPart{
		assistanttest.ToolCallScript("c1", "add_tile", badArgs),
		assistanttest.ToolCallScript("c2", "add_tile", badArgs),
		assistanttest.ToolCallScript("c3", "add_tile", badArgs),
		assistanttest.TextScript("I could not build that tile."),
	})

	chunks := collectTurn(t, svc, "conv_fail", "add nonsense")

	done := chunks[len(chunks)-1].GetDone()
	if done == nil {
		t.Fatal("missing done chunk")
	}
	if len(done.GetFailed()) != 1 || done.GetFailed()[0].GetIntent() != "nonsense" {
		t.Fatalf("failed = %v", done.GetFailed())
	}
	// A parse-abandoned tile produces NO op chunk — TurnDone.failed is the
	// only place it surfaces.
	for _, c := range chunks {
		if c.GetOp() != nil {
			t.Fatalf("unexpected op chunk: %v", c)
		}
	}
}

// Fail closed: an unreachable Redis must fail the turn before any model call,
// not silently serve an empty history.
func TestServiceTurn_HistoryLoadFailureFailsClosed(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	deadRedis := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond})
	t.Cleanup(func() { _ = deadRedis.Close() })
	svc, model := newTestService(deadRedis, nil)

	var emitted int
	err := svc.Turn(context.Background(), "conv_dead", nil, "hi", serviceTestCreds,
		func(*aidashboardsv1.TurnResponse) error { emitted++; return nil })

	if !errors.Is(err, ErrHistoryLoad) {
		t.Fatalf("err = %v, want ErrHistoryLoad", err)
	}
	if emitted != 0 {
		t.Fatalf("emitted %d chunks before failing", emitted)
	}
	if len(model.Calls) != 0 {
		t.Fatal("model must not be called when history is unavailable")
	}
}
