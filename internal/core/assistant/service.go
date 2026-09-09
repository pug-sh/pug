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
	// An incomplete scope is an identity defect, not an internal fault — the
	// handler maps it to Unauthenticated so a client can tell it apart.
	ErrIncompleteScope = errors.New("assistant: incomplete caller scope")
	// Another turn holds this conversation's lock; the handler maps it to Aborted.
	ErrTurnInProgress = errors.New("assistant: turn already in progress")
)

// turnTimeout bounds a whole turn. A stream has no deadline of its own, and
// maxSteps rounds of insight calls compose to far longer than any real turn.
const turnTimeout = 5 * time.Minute

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

	ctx, cancel := context.WithTimeout(ctx, turnTimeout)
	defer cancel()

	// Fail closed: an unscoped key would put this turn in a namespace shared
	// with every other caller whose scope is likewise incomplete.
	if missing := creds.missingField(); missing != "" {
		err := fmt.Errorf("%w: %s", ErrIncompleteScope, missing)
		slog.ErrorContext(ctx, "assistant turn rejected", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}

	release, err := acquireTurn(ctx, s.rdb, creds, conversationID)
	if errors.Is(err, ErrTurnInProgress) {
		slog.WarnContext(ctx, "assistant turn rejected: conversation busy", slog.String("project_id", creds.ProjectID))
		return err
	}
	if err != nil {
		slog.ErrorContext(ctx, "conversation turn lock failed", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return fmt.Errorf("%w: %w", ErrHistoryLoad, err)
	}
	defer release()

	history, err := loadHistory(ctx, s.rdb, creds, conversationID)
	if err != nil {
		slog.ErrorContext(ctx, "conversation history load failed", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return fmt.Errorf("%w: %w", ErrHistoryLoad, err)
	}

	insightTools := buildInsightTools(s.insights, creds)

	var sink []emittedOp
	opTools := buildOpTools(draft, &sink)

	var toolTrace []ToolCallRecord
	reply, err := runLoop(ctx, s.model, s.callOpts, draft, history, message,
		insightTools, opTools, &toolTrace,
		func(delta string) error { return emit(textChunk(delta)) })
	if err != nil {
		logTurnFailure(ctx, "assistant turn failed", err)
		return err
	}

	var ops []*aidashboardsv1.TileOp
	var failed []*aidashboardsv1.FailedOp
	for _, entry := range sink {
		if entry.op != nil {
			ops = append(ops, entry.op)
		}
		if entry.failed != nil {
			failed = append(failed, entry.failed)
		}
	}

	// Ahead of op emission: an op the client has applied is a durable draft
	// change, so a save failure has to precede it.
	updated := append(history,
		&aidashboardsv1.Message{Role: aidashboardsv1.Message_ROLE_USER.Enum(), Content: proto.String(message)},
		&aidashboardsv1.Message{Role: aidashboardsv1.Message_ROLE_ASSISTANT.Enum(), Content: proto.String(reply)},
	)
	if err := saveHistory(ctx, s.rdb, creds, conversationID, updated); err != nil {
		slog.ErrorContext(ctx, "conversation history save failed", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return fmt.Errorf("%w: %w", ErrHistorySave, err)
	}

	// Written before the ops go out: an emit failure past this point diverges
	// the client from the history just committed, and the trace is the only
	// record of that turn.
	//
	// Best-effort: the debug trace is observability, not correctness. Losing
	// one trace entry to a transient Redis hiccup should not fail a turn that
	// otherwise succeeded.
	failedEntries := make([]FailedIntent, 0, len(failed))
	for _, f := range failed {
		failedEntries = append(failedEntries, FailedIntent{Intent: f.GetIntent(), Violations: f.GetViolations()})
	}
	if err := recordTurnTrace(ctx, s.rdb, creds, conversationID, TurnTrace{
		ProjectID:  creds.ProjectID,
		Message:    message,
		Reply:      reply,
		ToolCalls:  toolTrace,
		Ops:        len(ops),
		Failed:     failedEntries,
		Model:      s.modelDesc,
		DurationMs: time.Since(startedAt).Milliseconds(),
	}); err != nil {
		slog.ErrorContext(ctx, "debug trace write failed", slogx.Error(err))
		telemetry.RecordError(ctx, err)
	}

	// History is already committed, so a failure here leaves the client without
	// ops the conversation claims were made — the one turn worth recording.
	for _, op := range ops {
		if err := emit(opChunk(op)); err != nil {
			logTurnFailure(ctx, "op emission failed after history commit", err)
			return err
		}
	}

	slog.InfoContext(ctx, "turn complete",
		slog.String("project_id", creds.ProjectID), slog.Int("ops", len(ops)), slog.Int("failed", len(failed)))

	return emit(doneChunk(failed))
}

// logTurnFailure records a turn failure, except that a client disconnect is
// the client's doing: a warning, not an exception on the span.
func logTurnFailure(ctx context.Context, msg string, err error) {
	if errors.Is(ctx.Err(), context.Canceled) {
		slog.WarnContext(ctx, msg+" (client disconnected)", slogx.Error(err))
		return
	}
	slog.ErrorContext(ctx, msg, slogx.Error(err))
	telemetry.RecordError(ctx, err)
}
