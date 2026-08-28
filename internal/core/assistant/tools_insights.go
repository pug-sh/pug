package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	aisdk "github.com/grafana/ai-sdk"
	"github.com/grafana/ai-sdk/schema"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/pug-sh/pug/internal/deps/telemetry"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1/insightsv1connect"
	"github.com/pug-sh/pug/internal/slogx"
)

// CallerCredentials is the caller's forwarded identity for the turn. The
// service holds no credentials of its own and has no standing access to any
// project — every backend call is scoped by these, so authorization is exactly
// the user's; a viewer cannot escalate through the assistant.
//
// CustomerID is the JWT subject. It is never forwarded anywhere: its only job
// is to scope this caller's Redis keys.
type CallerCredentials struct {
	JWT        string
	ProjectID  string
	CustomerID string
}

// missingField names the first empty field, or "" when the scope is complete.
func (c CallerCredentials) missingField() string {
	switch {
	case c.JWT == "":
		return "jwt"
	case c.ProjectID == "":
		return "project_id"
	case c.CustomerID == "":
		return "customer_id"
	}
	return ""
}

// mustSchema compiles a JSON Schema literal at package init. Panics only on a
// malformed literal — a programmer error caught by any test run, like
// regexp.MustCompile.
func mustSchema(raw string) schema.Schema {
	s, err := schema.SchemaFromJSON(json.RawMessage(raw))
	if err != nil {
		panic(fmt.Sprintf("assistant: invalid tool schema: %v", err))
	}
	return s
}

// jsonString wraps a tool's reply as the JSON string the SDK expects from
// Execute. Tool replies are always strings, as in the TS service.
func jsonString(s string) (json.RawMessage, error) {
	return json.Marshal(s)
}

// safely converts a failure into a model-visible "ERROR: ..." string. A tool
// must never return a Go error: the SDK would wrap it in its own error-result
// shape, and (in the TS SDK this was ported from) a thrown error aborted the
// whole turn.
//
// The model-visible string is the repair channel, not the operator one — an
// insights outage would otherwise be entirely silent — so failures are also
// logged here, the layer that detects them. Client-class codes are the model's
// to fix (a typo'd key, a project it cannot read) and stay at warn; the rest
// are ours and are recorded.
// maxToolResultBytes caps what one insight result feeds back to the model. The
// result rides in every later step of the turn and in the debug trace, and a
// fine-grained breakdown over a long window serialises to megabytes.
const maxToolResultBytes = 32 << 10

func capResult(s string) string {
	if len(s) <= maxToolResultBytes {
		return s
	}
	return s[:maxToolResultBytes] + "\n…[truncated: result too large — use a shorter window, coarser granularity or fewer breakdowns]"
}

func safely(ctx context.Context, tool string, fn func() (string, error)) (json.RawMessage, error) {
	out, err := fn()
	if err != nil {
		if modelRepairable(err) {
			slog.WarnContext(ctx, "insight tool rejected", slog.String("tool", tool), slogx.Error(err))
		} else {
			slog.ErrorContext(ctx, "insight tool failed", slog.String("tool", tool), slogx.Error(err))
			telemetry.RecordError(ctx, err)
		}
		return jsonString("ERROR: " + err.Error())
	}
	return jsonString(capResult(out))
}

// modelRepairable reports whether a failure is the model's to correct rather
// than an operator's. A bare (non-Connect) error is a local input/decode
// failure, which the model can also fix. Every code is named so a new one has
// to be classified rather than silently defaulting; anything unclassified falls
// through to false, the side that alerts.
func modelRepairable(err error) bool {
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		return true
	}
	switch cerr.Code() {
	// The model asked for something wrong, or for something this caller cannot
	// have. Both are answerable by calling differently; neither is an outage.
	case connect.CodeInvalidArgument, connect.CodeNotFound, connect.CodeAlreadyExists,
		connect.CodePermissionDenied, connect.CodeUnauthenticated,
		connect.CodeFailedPrecondition, connect.CodeOutOfRange, connect.CodeCanceled:
		return true
	case connect.CodeUnknown, connect.CodeDeadlineExceeded, connect.CodeResourceExhausted,
		connect.CodeAborted, connect.CodeUnimplemented, connect.CodeInternal,
		connect.CodeUnavailable, connect.CodeDataLoss:
		return false
	}
	return false
}

// authedRequest builds a Connect request carrying the caller's credentials.
// Header names match the main server's auth boundary ("x-project-id" is
// rpc.HeaderProjectID; not imported to keep core free of app packages).
func authedRequest[T any](msg *T, creds CallerCredentials) *connect.Request[T] {
	req := connect.NewRequest(msg)
	req.Header().Set("Authorization", "Bearer "+creds.JWT)
	req.Header().Set("x-project-id", creds.ProjectID)
	return req
}

var granularityByName = map[string]insightsv1.Granularity{
	"MINUTE": insightsv1.Granularity_GRANULARITY_MINUTE,
	"HOUR":   insightsv1.Granularity_GRANULARITY_HOUR,
	"DAY":    insightsv1.Granularity_GRANULARITY_DAY,
	"WEEK":   insightsv1.Granularity_GRANULARITY_WEEK,
	"MONTH":  insightsv1.Granularity_GRANULARITY_MONTH,
}

var propertySourceByName = map[string]commonv1.PropertySource{
	"AUTO":    commonv1.PropertySource_PROPERTY_SOURCE_AUTO,
	"CUSTOM":  commonv1.PropertySource_PROPERTY_SOURCE_CUSTOM,
	"PROFILE": commonv1.PropertySource_PROPERTY_SOURCE_PROFILE,
}

var filterSchemaInputSchema = mustSchema(`{
	"type": "object",
	"properties": {
		"eventKind": {"type": "string", "description": "Restrict property keys to those seen on this event kind. Empty for all."}
	}
}`)

var propertyValuesInputSchema = mustSchema(`{
	"type": "object",
	"properties": {
		"propertyKey": {"type": "string", "minLength": 1},
		"source": {"type": "string", "enum": ["AUTO", "CUSTOM", "PROFILE"]},
		"eventKind": {"type": "string"}
	},
	"required": ["propertyKey", "source"]
}`)

var queryInsightsInputSchema = mustSchema(`{
	"type": "object",
	"properties": {
		"spec": {"type": "object", "description": "An InsightQuerySpec as JSON — the insight.spec shape from the system prompt."},
		"fromIso": {"type": "string", "description": "Window start, RFC3339."},
		"toIso": {"type": "string", "description": "Window end, RFC3339."},
		"granularity": {"type": "string", "enum": ["MINUTE", "HOUR", "DAY", "WEEK", "MONTH"]}
	},
	"required": ["spec", "fromIso", "toIso", "granularity"]
}`)

// buildInsightTools builds the three read tools closed over (client, creds).
// Turn's scope guard has already rejected an incomplete creds, so there is no
// credential check here.
func buildInsightTools(client insightsv1connect.InsightsServiceClient, creds CallerCredentials) aisdk.ToolSet {
	return aisdk.ToolSet{
		// Description adapted from insights.proto GetFilterSchema — the same
		// text pug's MCP server ships as this tool's description.
		"get_insights_filter_schema": {
			Description: "Lists the event kinds and the property keys/types available to filter and break down by. " +
				"Call it before query_insights so that filters and breakdowns reference keys that actually " +
				"exist in this project.",
			InputSchema: filterSchemaInputSchema,
			Execute: func(ctx context.Context, input json.RawMessage, _ aisdk.ToolExecutionOptions) (json.RawMessage, error) {
				return safely(ctx, "get_insights_filter_schema", func() (string, error) {
					var args struct {
						EventKind string `json:"eventKind"`
					}
					if err := json.Unmarshal(input, &args); err != nil {
						return "", fmt.Errorf("invalid tool input: %w", err)
					}
					res, err := client.GetFilterSchema(ctx,
						authedRequest(&commonv1.GetFilterSchemaRequest{EventKind: &args.EventKind}, creds))
					if err != nil {
						return "", err
					}
					summary := struct {
						Events              []string `json:"events"`
						AutoPropertyKeys    []string `json:"autoPropertyKeys"`
						CustomPropertyKeys  []string `json:"customPropertyKeys"`
						ProfilePropertyKeys []string `json:"profilePropertyKeys"`
					}{
						Events:              make([]string, 0, len(res.Msg.GetEvents())),
						AutoPropertyKeys:    make([]string, 0, len(res.Msg.GetAutoPropertyKeys())),
						CustomPropertyKeys:  make([]string, 0, len(res.Msg.GetCustomPropertyKeys())),
						ProfilePropertyKeys: make([]string, 0, len(res.Msg.GetProfilePropertyKeys())),
					}
					for _, e := range res.Msg.GetEvents() {
						summary.Events = append(summary.Events, e.GetName())
					}
					for _, k := range res.Msg.GetAutoPropertyKeys() {
						summary.AutoPropertyKeys = append(summary.AutoPropertyKeys, k.GetName())
					}
					for _, k := range res.Msg.GetCustomPropertyKeys() {
						summary.CustomPropertyKeys = append(summary.CustomPropertyKeys, k.GetName())
					}
					for _, k := range res.Msg.GetProfilePropertyKeys() {
						summary.ProfilePropertyKeys = append(summary.ProfilePropertyKeys, k.GetName())
					}
					out, err := json.Marshal(summary)
					if err != nil {
						return "", err
					}
					return string(out), nil
				})
			},
		},

		// Description adapted from insights.proto GetPropertyValues.
		"get_insights_property_values": {
			Description: "Lists the observed values of an event or user property (e.g. the countries seen for " +
				"\"$country\") so filters and breakdowns can be populated with real values before running " +
				"query_insights.",
			InputSchema: propertyValuesInputSchema,
			Execute: func(ctx context.Context, input json.RawMessage, _ aisdk.ToolExecutionOptions) (json.RawMessage, error) {
				return safely(ctx, "get_insights_property_values", func() (string, error) {
					var args struct {
						PropertyKey string `json:"propertyKey"`
						Source      string `json:"source"`
						EventKind   string `json:"eventKind"`
					}
					if err := json.Unmarshal(input, &args); err != nil {
						return "", fmt.Errorf("invalid tool input: %w", err)
					}
					source, ok := propertySourceByName[args.Source]
					if !ok {
						return "", errors.New("source must be one of AUTO, CUSTOM, PROFILE")
					}
					res, err := client.GetPropertyValues(ctx, authedRequest(&insightsv1.GetPropertyValuesRequest{
						PropertyKey: &args.PropertyKey,
						Source:      source.Enum(),
						EventKind:   &args.EventKind,
					}, creds))
					if err != nil {
						return "", err
					}
					values := res.Msg.GetValues()
					if values == nil {
						values = []string{}
					}
					out, err := json.Marshal(values)
					if err != nil {
						return "", err
					}
					return string(out), nil
				})
			},
		},

		// Description adapted from insights.proto Query.
		"query_insights": {
			Description: "Runs a product-analytics insight over the project events and returns the computed result. " +
				"Use it to preview a candidate tile spec before proposing it — an empty result usually " +
				"means the event kind or property does not match this project. Discover valid event kinds " +
				"and property keys with get_insights_filter_schema first.",
			InputSchema: queryInsightsInputSchema,
			Execute: func(ctx context.Context, input json.RawMessage, _ aisdk.ToolExecutionOptions) (json.RawMessage, error) {
				return safely(ctx, "query_insights", func() (string, error) {
					var args struct {
						Spec        json.RawMessage `json:"spec"`
						FromIso     string          `json:"fromIso"`
						ToIso       string          `json:"toIso"`
						Granularity string          `json:"granularity"`
					}
					if err := json.Unmarshal(input, &args); err != nil {
						return "", fmt.Errorf("invalid tool input: %w", err)
					}
					from, errFrom := time.Parse(time.RFC3339, args.FromIso)
					to, errTo := time.Parse(time.RFC3339, args.ToIso)
					if errFrom != nil || errTo != nil {
						return "", errors.New("fromIso and toIso must be RFC3339 timestamps")
					}
					granularity, ok := granularityByName[args.Granularity]
					if !ok {
						return "", errors.New("granularity must be one of MINUTE, HOUR, DAY, WEEK, MONTH")
					}
					spec := &insightsv1.InsightQuerySpec{}
					if err := protojson.Unmarshal(args.Spec, spec); err != nil {
						return "", fmt.Errorf("spec is not a valid InsightQuerySpec: %w", err)
					}
					res, err := client.Query(ctx, authedRequest(&insightsv1.QueryRequest{
						Spec:        spec,
						TimeRange:   &commonv1.TimeRange{From: timestamppb.New(from), To: timestamppb.New(to)},
						Granularity: granularity.Enum(),
					}, creds))
					if err != nil {
						return "", err
					}
					out, err := protojson.Marshal(res.Msg)
					if err != nil {
						return "", err
					}
					return string(out), nil
				})
			},
		},
	}
}
