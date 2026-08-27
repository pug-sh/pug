package ai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"
	"github.com/grafana/ai-sdk/provider"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/pug-sh/pug/internal/core/assistant"
	"github.com/pug-sh/pug/internal/core/assistant/assistanttest"
	aidashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/ai/dashboards/v1"
	"github.com/pug-sh/pug/internal/gen/proto/ai/dashboards/v1/aidashboardsv1connect"
	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
	"github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1/insightsv1connect"
	"github.com/pug-sh/pug/internal/testutil"
)

// unusedInsightsClient satisfies the constructor; the scripted turns in these
// tests never call an insight tool.
func unusedInsightsClient(t *testing.T) insightsv1connect.InsightsServiceClient {
	t.Helper()
	return insightsv1connect.NewInsightsServiceClient(http.DefaultClient, "http://insights.invalid")
}

func newTestServer(t *testing.T, rdb *redis.Client, scripts [][]provider.StreamPart) (*httptest.Server, aidashboardsv1connect.DashboardAssistantServiceClient) {
	t.Helper()
	model := &assistanttest.ScriptedModel{Scripts: scripts}
	svc := assistant.NewService(rdb, unusedInsightsClient(t), model, nil, "agent=test:scripted")

	otelI, err := otelconnect.NewInterceptor()
	if err != nil {
		t.Fatalf("otel interceptor: %v", err)
	}
	mux := buildMux(t.Context(), svc, testJWTKey, []string{"*"}, otelI, func(ctx context.Context) error {
		return rdb.Ping(ctx).Err()
	})

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	client := aidashboardsv1connect.NewDashboardAssistantServiceClient(http.DefaultClient, ts.URL)
	return ts, client
}

func turnRequest(conversationID, message string) *connect.Request[aidashboardsv1.TurnRequest] {
	return connect.NewRequest(&aidashboardsv1.TurnRequest{
		ConversationId: proto.String(conversationID),
		State: &aidashboardsv1.ConversationState{
			Draft: &dashboardsv1.Dashboard{DisplayName: proto.String("My board")},
		},
		Message: proto.String(message),
	})
}

func authedTurnRequest(t *testing.T, conversationID, message string) *connect.Request[aidashboardsv1.TurnRequest] {
	t.Helper()
	req := turnRequest(conversationID, message)
	req.Header().Set("Authorization", "Bearer "+mintTestJWT(t, testJWTKey, nil))
	req.Header().Set("x-project-id", "prj_1")
	return req
}

func receiveAll(t *testing.T, stream *connect.ServerStreamForClient[aidashboardsv1.TurnResponse]) ([]*aidashboardsv1.TurnResponse, error) {
	t.Helper()
	var out []*aidashboardsv1.TurnResponse
	for stream.Receive() {
		out = append(out, proto.Clone(stream.Msg()).(*aidashboardsv1.TurnResponse))
	}
	return out, stream.Err()
}

func TestTurn_RejectsMissingAuth(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	_, client := newTestServer(t, rd.Client, nil)

	stream, err := client.Turn(context.Background(), turnRequest("conv_1", "hi"))
	if err == nil {
		// Streaming clients may defer the error to the first Receive.
		_, err = receiveAll(t, stream)
	}
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("code = %v (err=%v)", connect.CodeOf(err), err)
	}
}

func TestTurn_RejectsBadSignatureAndGarbage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	_, client := newTestServer(t, rd.Client, nil)

	for name, token := range map[string]string{
		"bad-signature": mintTestJWT(t, []byte("other"), nil),
		"garbage":       "not.a.jwt",
	} {
		req := turnRequest("conv_1", "hi")
		req.Header().Set("Authorization", "Bearer "+token)
		req.Header().Set("x-project-id", "prj_1")
		stream, err := client.Turn(context.Background(), req)
		if err == nil {
			_, err = receiveAll(t, stream)
		}
		if connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Fatalf("%s: code = %v", name, connect.CodeOf(err))
		}
	}
}

func TestTurn_ValidateInterceptorRejectsBadRequests(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	_, client := newTestServer(t, rd.Client, nil)

	// Empty message violates TurnRequest.message min_len; nil state violates
	// the required rule from Task 1; empty conversation_id violates min_len.
	// All must be CodeInvalidArgument from the validate interceptor — the
	// handler never runs.
	bad := []*aidashboardsv1.TurnRequest{
		{ConversationId: proto.String("c"), State: &aidashboardsv1.ConversationState{}, Message: proto.String("")},
		{ConversationId: proto.String("c"), Message: proto.String("hi")},
		{ConversationId: proto.String(""), State: &aidashboardsv1.ConversationState{}, Message: proto.String("hi")},
	}
	for i, msg := range bad {
		req := connect.NewRequest(msg)
		req.Header().Set("Authorization", "Bearer "+mintTestJWT(t, testJWTKey, nil))
		req.Header().Set("x-project-id", "prj_1")
		stream, err := client.Turn(context.Background(), req)
		if err == nil {
			_, err = receiveAll(t, stream)
		}
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("case %d: code = %v (err=%v)", i, connect.CodeOf(err), err)
		}
	}
}

func TestTurn_HappyPathStreamsInContractOrder(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	_, client := newTestServer(t, rd.Client, [][]provider.StreamPart{
		assistanttest.ToolCallScript("c1", "add_tile",
			`{"intent":"actives","tile":{"displayName":"Weekly actives","insight":{"spec":{"insightType":"INSIGHT_TYPE_TRENDS","events":[{"event":{"kind":"page_view"}}]}}}}`),
		assistanttest.TextScript("Added it."),
	})

	stream, err := client.Turn(context.Background(), authedTurnRequest(t, "conv_happy", "add a tile"))
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	chunks, err := receiveAll(t, stream)
	if err != nil {
		t.Fatalf("stream err: %v", err)
	}

	var kinds []string
	var text strings.Builder
	for _, c := range chunks {
		switch {
		case c.GetText() != "":
			kinds = append(kinds, "text")
			text.WriteString(c.GetText())
		case c.GetOp() != nil:
			kinds = append(kinds, "op")
			if c.GetOp().GetAdd().GetTile().GetPosition() == nil {
				t.Fatal("op tile missing assigned position")
			}
		case c.GetDone() != nil:
			kinds = append(kinds, "done")
		}
	}
	if got := strings.Join(kinds, ","); got != "text,op,done" {
		t.Fatalf("chunk order = %s", got)
	}
	if text.String() != "Added it." {
		t.Fatalf("text = %q", text.String())
	}

	// The conversation persisted server-side under the client's id.
	raw, err := rd.Client.Get(context.Background(), "conversation:conv_happy:messages").Result()
	if err != nil {
		t.Fatalf("history read: %v", err)
	}
	if !strings.Contains(raw, "add a tile") || !strings.Contains(raw, "Added it.") {
		t.Fatalf("history = %s", raw)
	}
}

func TestTurn_RedisOutageIsUnavailableWithExplicitMessage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	deadRedis := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond})
	t.Cleanup(func() { _ = deadRedis.Close() })
	_, client := newTestServer(t, deadRedis, nil)

	stream, err := client.Turn(context.Background(), authedTurnRequest(t, "conv_dead", "hi"))
	if err == nil {
		_, err = receiveAll(t, stream)
	}
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("code = %v (err=%v)", connect.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "conversation storage unavailable (load)") {
		t.Fatalf("err = %v", err)
	}
}

func TestHealthEndpoints(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	rd := testutil.SetupRedis(t)
	ts, _ := newTestServer(t, rd.Client, nil)

	res, err := http.Get(ts.URL + "/healthz")
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("healthz: %v %v", err, res)
	}
	res, err = http.Get(ts.URL + "/readyz")
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("readyz: %v %v", err, res)
	}
}

func TestReadyzFailsWhenRedisDown(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	deadRedis := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond})
	t.Cleanup(func() { _ = deadRedis.Close() })
	ts, _ := newTestServer(t, deadRedis, nil)

	res, err := http.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatalf("readyz: %v", err)
	}
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.StatusCode)
	}
}
