package assistant

import (
	"context"
	"maps"
	"strings"
	"sync"

	aisdk "github.com/grafana/ai-sdk"
	"github.com/grafana/ai-sdk/provider"

	aidashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/ai/dashboards/v1"
	dashboardsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/dashboards/v1"
)

// CallOptions are ai-sdk per-call options resolved at boot from the model
// route (sampling parameters). A named type so service.go never has to import
// the alpha SDK — ai-sdk imports stay confined to loop.go and llm.go.
type CallOptions []aisdk.StreamOption

// maxSteps bounds the tool loop, as in the TS service (stopWhen:
// stepCountIs(12)). The SDK's default is ONE step — without this option the
// model would never see its first tool result.
const maxSteps = 12

// systemPrompt is ported verbatim from the TS service (loop.ts) — behavior
// parity depends on it.
const systemPrompt = `You help a user build a pug analytics dashboard by conversation.

pug is a product-analytics platform. A dashboard is tiles on a 72-column grid.
Each insight tile carries an InsightQuerySpec describing a trends, segmentation,
funnel, retention, user-flow or top-K query.

Ground yourself before proposing anything:
- Call get_insights_filter_schema first. Never guess an event kind or property
  key — a spec referencing something this project does not record produces an
  empty chart, not an error.
- Call get_insights_property_values when you need real values for a filter.
- Call query_insights to preview a spec when you are unsure it will return data.

Propose changes as operations. Only touch tiles you are actually changing;
tiles you do not mention are left alone. Never call remove_tile unless the user
asked for something to be removed.

Keep replies short. Say what you changed and why — not how the tools work.`

// buildSystemPrompt appends the tile shape and its cross-field rules to the
// prompt. Both are rendered from the generated types, so a proto change
// reshapes them instead of leaving a stale string behind.
var buildSystemPrompt = sync.OnceValue(func() string {
	return systemPrompt + `

A tile is proto3 JSON matching this schema:
` + tileShapeJSON() + `

The schema cannot express these rules, but a tile that breaks one is rejected:
` + tileRules()
})

// runLoop drives one model conversation: streams text deltas to onText as they
// arrive, records every tool result into toolTrace, and returns the
// accumulated reply once the stream has fully completed — at which point every
// tool call has settled, so the caller can drain the op sink knowing nothing
// more will arrive. Tools cannot yield into the response stream; that drain
// order (text, then ops, then done) is the FE's chunk-order contract.
func runLoop(
	ctx context.Context,
	model provider.LanguageModel,
	callOpts CallOptions,
	draft *dashboardsv1.Dashboard,
	history []*aidashboardsv1.Message,
	message string,
	insightTools aisdk.ToolSet,
	opTools aisdk.ToolSet,
	toolTrace *[]ToolCallRecord,
	onText func(delta string) error,
) (string, error) {
	msgs := make([]provider.Message, 0, len(history)+2)
	msgs = append(msgs, provider.UserText(summarizeDraft(draft)))
	for _, m := range history {
		// A turn that only called tools leaves an empty reply; the provider
		// forwards an empty text block and the API rejects the whole call.
		if m.GetContent() == "" {
			continue
		}
		if m.GetRole() == aidashboardsv1.Message_ROLE_ASSISTANT {
			msgs = append(msgs, provider.AssistantText(m.GetContent()))
		} else {
			msgs = append(msgs, provider.UserText(m.GetContent()))
		}
	}
	msgs = append(msgs, provider.UserText(message))

	tools := make(aisdk.ToolSet, len(insightTools)+len(opTools))
	maps.Copy(tools, insightTools)
	maps.Copy(tools, opTools)

	opts := make([]aisdk.StreamOption, 0, len(callOpts)+5)
	opts = append(opts,
		aisdk.WithSystemMessages(aisdk.SystemModelMessage{
			Content:         buildSystemPrompt(),
			ProviderOptions: systemCacheOptions,
		}),
		aisdk.WithModelMessages(msgs...),
		aisdk.WithTools(tools),
		aisdk.WithStopWhen(aisdk.StepCountIs(maxSteps)),
		aisdk.OnStepEnd(func(step aisdk.OnStepFinishState) {
			for _, r := range step.ToolResults {
				*toolTrace = append(*toolTrace, ToolCallRecord{
					ToolName: r.ToolName,
					Input:    r.Input,
					Output:   r.Output,
				})
			}
		}),
	)
	for _, o := range callOpts {
		opts = append(opts, o)
	}

	result := aisdk.StreamText(ctx, model, opts...)

	var reply strings.Builder
	var emitErr error
	// FullStream must be fully drained even after an emit failure: the SDK's
	// producer blocks once its buffer fills, and complete drainage is also the
	// completion signal (all steps and tool calls settled).
	for part := range result.FullStream() {
		if emitErr != nil {
			continue
		}
		delta, ok := part.(aisdk.StreamTextDelta)
		if !ok {
			continue
		}
		reply.WriteString(delta.Text)
		if err := onText(delta.Text); err != nil {
			emitErr = err
		}
	}
	if emitErr != nil {
		return reply.String(), emitErr
	}
	// Tool errors deliberately do NOT surface here (they were returned to the
	// model as strings); Err() carries provider/orchestration failures only.
	if err := result.Err(); err != nil {
		return reply.String(), err
	}
	return reply.String(), nil
}
