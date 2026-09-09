package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/grafana/ai-sdk/provider"
	"github.com/redis/go-redis/v9"

	"github.com/pug-sh/pug/internal/core/assistant/assistanttest"
	aidashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/ai/dashboards/v1"
	"github.com/pug-sh/pug/internal/testutil"
)

var serviceTestCreds = CallerCredentials{JWT: "test-jwt-value", ProjectID: "prj_1", CustomerID: "cus_1"}

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
	ttl, err := rd.Client.TTL(context.Background(), historyKey(serviceTestCreds, "conv_hist")).Result()
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

	raw, err := rd.Client.LIndex(context.Background(), traceKey(serviceTestCreds, "conv_trace"), 0).Result()
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

func TestTurn_RejectsIncompleteCallerScope(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	svc, model := newTestService(rd.Client, [][]provider.StreamPart{assistanttest.TextScript("hi")})

	for name, creds := range map[string]CallerCredentials{
		"no jwt":      {ProjectID: "prj_1", CustomerID: "cus_1"},
		"no project":  {JWT: "j", CustomerID: "cus_1"},
		"no customer": {JWT: "j", ProjectID: "prj_1"},
	} {
		var emitted int
		err := svc.Turn(context.Background(), "conv", nil, "hi", creds,
			func(*aidashboardsv1.TurnResponse) error { emitted++; return nil })

		// Identity, not merely non-nil: the guard has to be what rejected this,
		// and it has to run before Redis or the model is touched.
		if !errors.Is(err, ErrIncompleteScope) {
			t.Fatalf("%s: err = %v, want ErrIncompleteScope", name, err)
		}
		if emitted != 0 {
			t.Fatalf("%s: emitted %d chunks", name, emitted)
		}
		if len(model.Calls) != 0 {
			t.Fatalf("%s: model was called with an unscoped caller", name)
		}
	}
}

// failSetHook fails only a plain SET (not the turn lock's SET NX), so history
// loads normally and the save at the end of the turn is the single thing that
// breaks.
type failSetHook struct{}

func (failSetHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (failSetHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "set" && !slices.Contains(cmd.Args(), any("nx")) {
			return errors.New("redis: set refused")
		}
		return next(ctx, cmd)
	}
}

func (failSetHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// A save failure must fail the turn before any op reaches the client —
// otherwise the client applies tiles the persisted conversation never records.
func TestServiceTurn_HistorySaveFailureEmitsNoOps(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	rd.Client.AddHook(failSetHook{})
	svc, _ := newTestService(rd.Client, [][]provider.StreamPart{
		assistanttest.ToolCallScript("c1", "add_tile", fmt.Sprintf(`{"intent":"actives","tile":%s}`, validTileJSON)),
		assistanttest.TextScript("Added it."),
	})

	var ops, done int
	err := svc.Turn(context.Background(), "conv_save_fail", nil, "add a tile", serviceTestCreds,
		func(chunk *aidashboardsv1.TurnResponse) error {
			if chunk.GetOp() != nil {
				ops++
			}
			if chunk.GetDone() != nil {
				done++
			}
			return nil
		})

	if !errors.Is(err, ErrHistorySave) {
		t.Fatalf("err = %v, want ErrHistorySave", err)
	}
	if ops != 0 || done != 0 {
		t.Fatalf("emitted %d ops and %d done chunks after a failed save", ops, done)
	}
}

// History is committed by the time ops go out, so an emit failure there is the
// one turn that diverges the client from the record — it must still be traced.
func TestServiceTurn_EmitFailureAfterCommitIsStillTraced(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	svc, _ := newTestService(rd.Client, [][]provider.StreamPart{
		assistanttest.ToolCallScript("c1", "add_tile", fmt.Sprintf(`{"intent":"actives","tile":%s}`, validTileJSON)),
		assistanttest.TextScript("Added it."),
	})

	boom := errors.New("client gone")
	err := svc.Turn(context.Background(), "conv_emit_fail", nil, "add a tile", serviceTestCreds,
		func(chunk *aidashboardsv1.TurnResponse) error {
			if chunk.GetOp() != nil {
				return boom
			}
			return nil
		})

	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the emit error", err)
	}
	entries, err := rd.Client.LRange(context.Background(),
		traceKey(serviceTestCreds, "conv_emit_fail"), 0, -1).Result()
	if err != nil {
		t.Fatalf("lrange: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("%d traces, want 1 — the diverged turn left no record", len(entries))
	}
}

// A stream carries no deadline of its own, so the turn imposes one — otherwise
// maxSteps rounds of insight calls compose to far longer than any real turn.
func TestServiceTurn_BoundsItsOwnDuration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	svc, model := newTestService(rd.Client, [][]provider.StreamPart{assistanttest.TextScript("hi")})

	collectTurn(t, svc, "conv_deadline", "hi")

	deadline := model.CallDeadlines[0]
	if deadline.IsZero() {
		t.Fatal("the model ran with no deadline")
	}
	if left := time.Until(deadline); left <= 0 || left > turnTimeout {
		t.Fatalf("deadline is %v away, want within %v", left, turnTimeout)
	}
}

func TestServiceTurn_RejectsAConcurrentTurnOnTheSameConversation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	svc, _ := newTestService(rd.Client, [][]provider.StreamPart{assistanttest.TextScript("ok")})
	ctx := context.Background()

	if err := rd.Client.Set(ctx, turnLockKey(serviceTestCreds, "conv_busy"), "1", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	err := svc.Turn(ctx, "conv_busy", nil, "hi", serviceTestCreds,
		func(*aidashboardsv1.TurnResponse) error { return nil })
	if !errors.Is(err, ErrTurnInProgress) {
		t.Fatalf("err = %v, want ErrTurnInProgress", err)
	}

	// A completed turn releases the lock.
	collectTurn(t, svc, "conv_free", "hi")
	if n, _ := rd.Client.Exists(ctx, turnLockKey(serviceTestCreds, "conv_free")).Result(); n != 0 {
		t.Fatal("turn lock not released")
	}
}
