// Package assistant implements the dashboard assistant: one user message
// against a client-owned dashboard draft in, streamed assistant prose plus
// validated TileOps out. Conversation history is persisted server-side in
// Redis; every insights read happens over the shared Connect API with the
// caller's forwarded JWT, so authorization is exactly the user's own.
package assistant

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/grafana/ai-sdk/provider"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/pug-sh/pug/internal/deps/telemetry"
	aidashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/ai/dashboards/v1"
	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
	"github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1/insightsv1connect"
	"github.com/pug-sh/pug/internal/slogx"
)

// History load/save is load-bearing once conversation state lives in Redis: an
// outage must fail the turn (the handler maps these to CodeUnavailable), not
// silently serve an empty history that would look to the user like the model
// forgot everything.
var (
	ErrHistoryLoad = errors.New("assistant: conversation history load failed")
	ErrHistorySave = errors.New("assistant: conversation history save failed")
)

// Service orchestrates assistant turns. It holds no caller credentials — they
// arrive per turn and are never stored or logged.
type Service struct {
	rdb       *redis.Client
	insights  insightsv1connect.InsightsServiceClient
	model     provider.LanguageModel
	callOpts  CallOptions
	modelDesc string
}

func NewService(
	rdb *redis.Client,
	insights insightsv1connect.InsightsServiceClient,
	model provider.LanguageModel,
	callOpts CallOptions,
	modelDesc string,
) *Service {
	return &Service{rdb: rdb, insights: insights, model: model, callOpts: callOpts, modelDesc: modelDesc}
}

func textChunk(delta string) *aidashboardsv1.TurnResponse {
	return &aidashboardsv1.TurnResponse{Chunk: &aidashboardsv1.TurnResponse_Text{Text: delta}}
}

func opChunk(op *aidashboardsv1.TileOp) *aidashboardsv1.TurnResponse {
	return &aidashboardsv1.TurnResponse{Chunk: &aidashboardsv1.TurnResponse_Op{Op: op}}
}

func doneChunk(failed []*aidashboardsv1.FailedOp) *aidashboardsv1.TurnResponse {
	return &aidashboardsv1.TurnResponse{Chunk: &aidashboardsv1.TurnResponse_Done{
		Done: &aidashboardsv1.TurnDone{Failed: failed},
	}}
}

// Turn processes one user message against the draft and emits response chunks
// in the FE's contract order: text deltas while the model streams, then ops
// (drained after the stream completes — tools cannot yield mid-stream), then
// exactly one done chunk.
func (s *Service) Turn(
	ctx context.Context,
	conversationID string,
	draft *dashboardsv1.Dashboard,
	message string,
	creds CallerCredentials,
	emit func(*aidashboardsv1.TurnResponse) error,
) error {
	startedAt := time.Now()

	history, err := loadHistory(ctx, s.rdb, conversationID)
	if err != nil {
		slog.ErrorContext(ctx, "conversation history load failed", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return fmt.Errorf("%w: %w", ErrHistoryLoad, err)
	}

	// Built once per turn so a missing credential fails at setup, not midway
	// through the model's first tool call. Unreachable when the authn boundary
	// did its job (it requires both values), so this is defense in depth.
	insightTools, err := buildInsightTools(s.insights, creds)
	if err != nil {
		slog.ErrorContext(ctx, "assistant tool setup failed", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}

	var sink []emittedOp
	opTools := buildOpTools(draft, &sink)

	var toolTrace []ToolCallRecord
	reply, err := runLoop(ctx, s.model, s.callOpts, draft, history, message,
		insightTools, opTools, &toolTrace,
		func(delta string) error { return emit(textChunk(delta)) })
	if err != nil {
		slog.ErrorContext(ctx, "assistant turn failed", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}

	var failed []*aidashboardsv1.FailedOp
	ops := 0
	for _, entry := range sink {
		if entry.op != nil {
			ops++
			if err := emit(opChunk(entry.op)); err != nil {
				return err
			}
		}
		if entry.failed != nil {
			failed = append(failed, entry.failed)
		}
	}

	updated := append(history,
		&aidashboardsv1.Message{Role: aidashboardsv1.Message_ROLE_USER.Enum(), Content: proto.String(message)},
		&aidashboardsv1.Message{Role: aidashboardsv1.Message_ROLE_ASSISTANT.Enum(), Content: proto.String(reply)},
	)
	if err := saveHistory(ctx, s.rdb, conversationID, updated); err != nil {
		slog.ErrorContext(ctx, "conversation history save failed", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return fmt.Errorf("%w: %w", ErrHistorySave, err)
	}

	// Best-effort: the debug trace is observability, not correctness. Losing
	// one trace entry to a transient Redis hiccup should not fail a turn that
	// otherwise succeeded.
	failedEntries := make([]FailedIntent, 0, len(failed))
	for _, f := range failed {
		failedEntries = append(failedEntries, FailedIntent{Intent: f.GetIntent(), Violations: f.GetViolations()})
	}
	if err := recordTurnTrace(ctx, s.rdb, conversationID, TurnTrace{
		ProjectID:  creds.ProjectID,
		Message:    message,
		Reply:      reply,
		ToolCalls:  toolTrace,
		Ops:        ops,
		Failed:     failedEntries,
		Model:      s.modelDesc,
		DurationMs: time.Since(startedAt).Milliseconds(),
	}); err != nil {
		slog.ErrorContext(ctx, "debug trace write failed", slogx.Error(err))
		telemetry.RecordError(ctx, err)
	}

	slog.InfoContext(ctx, "turn complete",
		slog.String("project_id", creds.ProjectID), slog.Int("ops", ops), slog.Int("failed", len(failed)))

	return emit(doneChunk(failed))
}
