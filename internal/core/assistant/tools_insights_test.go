package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"
	aisdk "github.com/grafana/ai-sdk"
	"google.golang.org/protobuf/proto"

	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1/insightsv1connect"
)

// fakeInsightsClient implements insightsv1connect.InsightsServiceClient with
// per-method function fields; unset methods fail the test if called.
type fakeInsightsClient struct {
	getFilterSchema   func(context.Context, *connect.Request[commonv1.GetFilterSchemaRequest]) (*connect.Response[commonv1.GetFilterSchemaResponse], error)
	getPropertyValues func(context.Context, *connect.Request[insightsv1.GetPropertyValuesRequest]) (*connect.Response[insightsv1.GetPropertyValuesResponse], error)
	query             func(context.Context, *connect.Request[insightsv1.QueryRequest]) (*connect.Response[insightsv1.QueryResponse], error)
}

var _ insightsv1connect.InsightsServiceClient = (*fakeInsightsClient)(nil)

func (f *fakeInsightsClient) Query(ctx context.Context, req *connect.Request[insightsv1.QueryRequest]) (*connect.Response[insightsv1.QueryResponse], error) {
	if f.query == nil {
		return nil, errors.New("unexpected Query call")
	}
	return f.query(ctx, req)
}

func (f *fakeInsightsClient) SegmentUsers(context.Context, *connect.Request[insightsv1.SegmentUsersRequest]) (*connect.Response[insightsv1.SegmentUsersResponse], error) {
	return nil, errors.New("unexpected SegmentUsers call")
}

func (f *fakeInsightsClient) GetFilterSchema(ctx context.Context, req *connect.Request[commonv1.GetFilterSchemaRequest]) (*connect.Response[commonv1.GetFilterSchemaResponse], error) {
	if f.getFilterSchema == nil {
		return nil, errors.New("unexpected GetFilterSchema call")
	}
	return f.getFilterSchema(ctx, req)
}

func (f *fakeInsightsClient) GetPropertyValues(ctx context.Context, req *connect.Request[insightsv1.GetPropertyValuesRequest]) (*connect.Response[insightsv1.GetPropertyValuesResponse], error) {
	if f.getPropertyValues == nil {
		return nil, errors.New("unexpected GetPropertyValues call")
	}
	return f.getPropertyValues(ctx, req)
}

func stubInsightsClient() *fakeInsightsClient {
	return &fakeInsightsClient{
		getFilterSchema: func(context.Context, *connect.Request[commonv1.GetFilterSchemaRequest]) (*connect.Response[commonv1.GetFilterSchemaResponse], error) {
			return connect.NewResponse(&commonv1.GetFilterSchemaResponse{
				Events: []*commonv1.EventNameMeta{
					{Name: proto.String("page_view")}, {Name: proto.String("signup")},
				},
				AutoPropertyKeys:   []*commonv1.PropertyKeyMeta{{Name: proto.String("$country")}},
				CustomPropertyKeys: []*commonv1.PropertyKeyMeta{{Name: proto.String("plan")}},
			}), nil
		},
		getPropertyValues: func(context.Context, *connect.Request[insightsv1.GetPropertyValuesRequest]) (*connect.Response[insightsv1.GetPropertyValuesResponse], error) {
			return connect.NewResponse(&insightsv1.GetPropertyValuesResponse{Values: []string{"IN", "US"}}), nil
		},
	}
}

var testCreds = CallerCredentials{JWT: "j", ProjectID: "p", CustomerID: "c"}

// execToolString runs a tool and decodes its JSON-string output.
func execToolString(t *testing.T, tool aisdk.Tool, args string) string {
	t.Helper()
	out, err := tool.Execute(context.Background(), json.RawMessage(args), aisdk.ToolExecutionOptions{})
	if err != nil {
		t.Fatalf("Execute returned an error — tool errors must be returned as strings: %v", err)
	}
	var s string
	if err := json.Unmarshal(out, &s); err != nil {
		t.Fatalf("tool output is not a JSON string: %v (%s)", err, out)
	}
	return s
}

func TestBuildInsightTools_ExposesExactlyThreeReadTools(t *testing.T) {
	tools := buildInsightTools(stubInsightsClient(), testCreds)
	for _, name := range []string{"get_insights_filter_schema", "get_insights_property_values", "query_insights"} {
		if _, ok := tools[name]; !ok {
			t.Fatalf("missing tool %s", name)
		}
	}
	if len(tools) != 3 {
		t.Fatalf("len = %d", len(tools))
	}
}

// Turn's guard is the single credential check; every field must be named so an
// operator learns which one was empty.
func TestCallerCredentials_MissingField(t *testing.T) {
	for want, creds := range map[string]CallerCredentials{
		"jwt":         {ProjectID: "p", CustomerID: "c"},
		"project_id":  {JWT: "j", CustomerID: "c"},
		"customer_id": {JWT: "j", ProjectID: "p"},
		"":            testCreds,
	} {
		if got := creds.missingField(); got != want {
			t.Fatalf("missingField() = %q, want %q", got, want)
		}
	}
}

func TestFilterSchemaTool_ReturnsKindsAndKeys(t *testing.T) {
	tools := buildInsightTools(stubInsightsClient(), testCreds)
	out := execToolString(t, tools["get_insights_filter_schema"], `{"eventKind":""}`)
	for _, want := range []string{"page_view", "$country", "plan"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q: %s", want, out)
		}
	}
}

func TestInsightTools_ForwardCallerHeaders(t *testing.T) {
	var gotAuth, gotProject string
	client := stubInsightsClient()
	inner := client.getFilterSchema
	client.getFilterSchema = func(ctx context.Context, req *connect.Request[commonv1.GetFilterSchemaRequest]) (*connect.Response[commonv1.GetFilterSchemaResponse], error) {
		gotAuth = req.Header().Get("Authorization")
		gotProject = req.Header().Get("x-project-id")
		return inner(ctx, req)
	}

	tools := buildInsightTools(client, testCreds)
	execToolString(t, tools["get_insights_filter_schema"], `{"eventKind":""}`)
	if gotAuth != "Bearer j" || gotProject != "p" {
		t.Fatalf("headers = %q / %q", gotAuth, gotProject)
	}
}

func TestPropertyValuesTool_ReturnsObservedValues(t *testing.T) {
	tools := buildInsightTools(stubInsightsClient(), testCreds)
	out := execToolString(t, tools["get_insights_property_values"],
		`{"propertyKey":"$country","source":"AUTO","eventKind":""}`)
	if !strings.Contains(out, "IN") || !strings.Contains(out, "US") {
		t.Fatalf("output = %s", out)
	}
}

// A tool that surfaced a Go error would get wrapped in the SDK's error-result
// shape; returning the error text keeps the model-visible format identical to
// the TS service and lets the model adapt.
func TestInsightTools_BackendErrorReturnedToModelNotThrown(t *testing.T) {
	client := stubInsightsClient()
	client.getFilterSchema = func(context.Context, *connect.Request[commonv1.GetFilterSchemaRequest]) (*connect.Response[commonv1.GetFilterSchemaResponse], error) {
		return nil, errors.New("permission denied")
	}
	tools := buildInsightTools(client, testCreds)
	out := execToolString(t, tools["get_insights_filter_schema"], `{"eventKind":""}`)
	if !strings.HasPrefix(out, "ERROR: ") || !strings.Contains(out, "permission denied") {
		t.Fatalf("output = %q", out)
	}
}

func TestQueryTool_RejectsNonRFC3339Window(t *testing.T) {
	tools := buildInsightTools(stubInsightsClient(), testCreds)
	out := execToolString(t, tools["query_insights"],
		`{"spec":{"insightType":"INSIGHT_TYPE_TRENDS"},"fromIso":"yesterday","toIso":"today","granularity":"DAY"}`)
	if out != "ERROR: fromIso and toIso must be RFC3339 timestamps" {
		t.Fatalf("output = %q", out)
	}
}

func TestQueryTool_RunsSpecAndReturnsResult(t *testing.T) {
	client := stubInsightsClient()
	var gotGranularity insightsv1.Granularity
	var gotKind string
	client.query = func(_ context.Context, req *connect.Request[insightsv1.QueryRequest]) (*connect.Response[insightsv1.QueryResponse], error) {
		gotGranularity = req.Msg.GetGranularity()
		if events := req.Msg.GetSpec().GetEvents(); len(events) > 0 {
			gotKind = events[0].GetEvent().GetKind()
		}
		return connect.NewResponse(&insightsv1.QueryResponse{
			Result: &insightsv1.QueryResponse_Segmentation{
				Segmentation: &insightsv1.SegmentationResult{},
			},
		}), nil
	}

	tools := buildInsightTools(client, testCreds)
	out := execToolString(t, tools["query_insights"],
		`{"spec":{"insightType":"INSIGHT_TYPE_SEGMENTATION","events":[{"event":{"kind":"page_view"}}]},"fromIso":"2026-08-01T00:00:00Z","toIso":"2026-08-08T00:00:00Z","granularity":"DAY"}`)
	if gotGranularity != insightsv1.Granularity_GRANULARITY_DAY {
		t.Fatalf("granularity = %v", gotGranularity)
	}
	if gotKind != "page_view" {
		t.Fatalf("event kind = %q", gotKind)
	}
	if !strings.Contains(out, "segmentation") {
		t.Fatalf("output = %s", out)
	}
}

func TestQueryTool_UnparseableSpecIsAnErrorString(t *testing.T) {
	tools := buildInsightTools(stubInsightsClient(), testCreds)
	out := execToolString(t, tools["query_insights"],
		`{"spec":{"notAField":1},"fromIso":"2026-08-01T00:00:00Z","toIso":"2026-08-08T00:00:00Z","granularity":"DAY"}`)
	if !strings.HasPrefix(out, "ERROR: ") {
		t.Fatalf("output = %q", out)
	}
}

// A failing insight tool must reach an operator, not only the model. The split
// is by disposition: the model's mistakes are warnings, ours are errors.
func TestModelRepairable_SplitsByDisposition(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"bad argument":  {connect.NewError(connect.CodeInvalidArgument, errors.New("x")), true},
		"not found":     {connect.NewError(connect.CodeNotFound, errors.New("x")), true},
		"forbidden":     {connect.NewError(connect.CodePermissionDenied, errors.New("x")), true},
		"upstream down": {connect.NewError(connect.CodeUnavailable, errors.New("x")), false},
		"timed out":     {connect.NewError(connect.CodeDeadlineExceeded, errors.New("x")), false},
		"internal":      {connect.NewError(connect.CodeInternal, errors.New("x")), false},
		"unimplemented": {connect.NewError(connect.CodeUnimplemented, errors.New("x")), false},
		"out of range":  {connect.NewError(connect.CodeOutOfRange, errors.New("x")), true},
		"local decode":  {errors.New("invalid tool input"), true},
		"wrapped":       {fmt.Errorf("q: %w", connect.NewError(connect.CodeUnavailable, errors.New("x"))), false},
	} {
		if got := modelRepairable(tc.err); got != tc.want {
			t.Fatalf("%s: modelRepairable = %v, want %v", name, got, tc.want)
		}
	}
}

func TestCapResult_TruncatesOversizedOutput(t *testing.T) {
	small := strings.Repeat("a", maxToolResultBytes)
	if capResult(small) != small {
		t.Fatal("result within the cap was altered")
	}
	got := capResult(small + "b")
	if !strings.HasPrefix(got, small) || !strings.Contains(got, "truncated") || strings.HasSuffix(got, "b") {
		t.Fatalf("capResult = ...%q", got[len(got)-40:])
	}
}
