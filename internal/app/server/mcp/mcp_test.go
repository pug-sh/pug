package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"connectrpc.com/validate"
	"github.com/jackc/pgx/v5"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
	pogrpc "github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/core/authz"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	"github.com/pug-sh/pug/internal/gen/proto/shared/activity/v1/activityv1connect"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1/insightsv1connect"
	insightsv1mcp "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1/insightsv1mcp"
	profilesv1 "github.com/pug-sh/pug/internal/gen/proto/shared/profiles/v1"
	"github.com/pug-sh/pug/internal/gen/proto/shared/profiles/v1/profilesv1connect"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	mcpruntime "github.com/redpanda-data/protoc-gen-go-mcp/pkg/runtime"
	"google.golang.org/protobuf/proto"
)

const testPrivateKey = "prv_testkey"

// curatedToolNames is the pinned public contract: exactly the tools /mcp exposes.
// Kept as a literal (not derived from toolRenames) so an accidental edit to the
// rename table is caught here.
var curatedToolNames = []string{
	"explore_events",
	"get_activity_feed",
	"get_activity_filter_schema",
	"get_activity_heatmap",
	"get_activity_property_values",
	"get_deletion_request",
	"get_insights_filter_schema",
	"get_insights_property_values",
	"get_profile",
	"get_profile_by_external_id",
	"get_profile_stats",
	"query_insights",
}

// ---------- stubs ----------

type stubKeyLookup struct{ project dbread.Project }

func (s *stubKeyLookup) GetProjectByPublicApiKey(context.Context, string) (dbread.Project, error) {
	return dbread.Project{}, pgx.ErrNoRows
}

func (s *stubKeyLookup) GetProjectByPrivateApiKey(_ context.Context, key string) (dbread.Project, error) {
	if key == s.project.PrivateApiKey {
		return s.project, nil
	}
	return dbread.Project{}, pgx.ErrNoRows
}

func (s *stubKeyLookup) InvalidateProjectKeys(context.Context, string, string) {}

// stubRoleLookup satisfies the authz interceptor's memberRoleLookup. On the
// private-key path the interceptor short-circuits without a role lookup, so this
// must never be called.
type stubRoleLookup struct{}

func (stubRoleLookup) GetMemberRole(context.Context, string, string) (coreorgs.Role, error) {
	return coreorgs.Role(""), errors.New("role lookup must not run on the private-key path")
}

type stubInsights struct {
	insightsv1connect.UnimplementedInsightsServiceHandler
	queryCalls int
}

func (s *stubInsights) Query(context.Context, *connect.Request[insightsv1.QueryRequest]) (*connect.Response[insightsv1.QueryResponse], error) {
	s.queryCalls++
	return connect.NewResponse(&insightsv1.QueryResponse{}), nil
}

type stubActivity struct {
	activityv1connect.UnimplementedActivityServiceHandler
}

type stubProfiles struct {
	profilesv1connect.UnimplementedProfilesServiceHandler
	gotID      string
	sawProject bool
	getResp    *profilesv1.GetResponse // if set, returned from Get
	getErr     error                   // if set, returned from Get (takes precedence)
}

func (s *stubProfiles) Get(ctx context.Context, req *connect.Request[profilesv1.GetRequest]) (*connect.Response[profilesv1.GetResponse], error) {
	s.gotID = req.Msg.GetId()
	if p, err := pogrpc.MustGetPrincipalWithProject(ctx); err == nil && p.Project != nil {
		s.sawProject = true
	}
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.getResp != nil {
		return connect.NewResponse(s.getResp), nil
	}
	return connect.NewResponse(&profilesv1.GetResponse{}), nil
}

// ---------- harness ----------

type testDeps struct {
	server   *httptest.Server
	insights *stubInsights
	profiles *stubProfiles
}

func newTestServer(t *testing.T) testDeps {
	t.Helper()

	insights := &stubInsights{}
	profiles := &stubProfiles{}
	repo := &stubKeyLookup{project: dbread.Project{ID: "proj-mcp", OrgID: "org-mcp", PrivateApiKey: testPrivateKey}}

	authorizer, err := authz.NewAuthorizer()
	if err != nil {
		t.Fatalf("authz.NewAuthorizer: %v", err)
	}

	// The real interceptor sandwich (minus otel, which needs SDK setup and does
	// not affect these assertions). validate proves protovalidate runs on the
	// loopback path; authz proves the API-key no-op path; principal proves the
	// handler sees an authenticated Principal.
	handlerOpts := connect.WithInterceptors(
		pogrpc.CorrelationInterceptor(),
		pogrpc.LoggingInterceptor(),
		pogrpc.ErrorInterceptor(),
		validate.NewInterceptor(validate.WithoutErrorDetails()),
		pogrpc.PrincipalInterceptor(),
		pogrpc.AuthzInterceptor(authorizer, stubRoleLookup{}),
	)

	mux := http.NewServeMux()
	sharedMW := authn.NewMiddleware(pogrpc.WithDualAuth([]byte("test-jwt-key"), nil, repo))

	ip, ih := insightsv1connect.NewInsightsServiceHandler(insights, handlerOpts)
	mux.Handle(ip, sharedMW.Wrap(ih))
	ap, ah := activityv1connect.NewActivityServiceHandler(&stubActivity{}, handlerOpts)
	mux.Handle(ap, sharedMW.Wrap(ah))
	pp, ph := profilesv1connect.NewProfilesServiceHandler(profiles, handlerOpts)
	mux.Handle(pp, sharedMW.Wrap(ph))

	mcpHandler, err := NewHandler(mux)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	Mount(mux, mcpHandler, pogrpc.WithPrivateKeyAuth(repo))

	srv := httptest.NewServer(pogrpc.WithCorrelationID(mux))
	t.Cleanup(srv.Close)
	return testDeps{server: srv, insights: insights, profiles: profiles}
}

type headerRoundTripper struct {
	base        http.RoundTripper
	name, value string
}

func (h headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set(h.name, h.value)
	return h.base.RoundTrip(req)
}

func connectMCP(t *testing.T, endpoint, header, value string) (*gomcp.ClientSession, context.Context) {
	t.Helper()
	transport := &gomcp.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           &http.Client{Transport: headerRoundTripper{base: http.DefaultTransport, name: header, value: value}},
		DisableStandaloneSSE: true,
	}
	client := gomcp.NewClient(&gomcp.Implementation{Name: "test", Version: "0"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, ctx
}

func toolText(res *gomcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*gomcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// ---------- e2e ----------

func TestE2E_ToolsListPinsCuratedNames(t *testing.T) {
	deps := newTestServer(t)
	session, ctx := connectMCP(t, deps.server.URL+"/mcp", pogrpc.HeaderAPIKey, testPrivateKey)

	res, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	got := make([]string, 0, len(res.Tools))
	for _, tl := range res.Tools {
		got = append(got, tl.Name)
	}
	sort.Strings(got)

	if strings.Join(got, ",") != strings.Join(curatedToolNames, ",") {
		t.Errorf("tools/list =\n  %v\nwant\n  %v", got, curatedToolNames)
	}
}

func TestE2E_GetProfileHappyPath(t *testing.T) {
	deps := newTestServer(t)
	session, ctx := connectMCP(t, deps.server.URL+"/mcp", pogrpc.HeaderAPIKey, testPrivateKey)

	res, err := session.CallTool(ctx, &gomcp.CallToolParams{
		Name:      "get_profile",
		Arguments: map[string]any{"id": "user-123"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got tool error: %s", toolText(res))
	}
	if deps.profiles.gotID != "user-123" {
		t.Errorf("handler saw id %q, want %q", deps.profiles.gotID, "user-123")
	}
	if !deps.profiles.sawProject {
		t.Error("handler did not see an authenticated Principal with a Project")
	}
}

func TestE2E_GetProfileReturnsBody(t *testing.T) {
	deps := newTestServer(t)
	// A populated response must round-trip through the loopback responseRecorder
	// and the generated forwarder into readable MCP tool text — the adapter's
	// whole job. The empty-response happy path can't catch a dropped body/header.
	deps.profiles.getResp = &profilesv1.GetResponse{
		Profile: &profilesv1.Profile{
			Id:         proto.String("user-123"),
			ExternalId: proto.String("ext-abc"),
		},
	}
	session, ctx := connectMCP(t, deps.server.URL+"/mcp", pogrpc.HeaderAPIKey, testPrivateKey)

	res, err := session.CallTool(ctx, &gomcp.CallToolParams{
		Name:      "get_profile",
		Arguments: map[string]any{"id": "user-123"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got tool error: %s", toolText(res))
	}
	if text := toolText(res); !strings.Contains(text, "ext-abc") {
		t.Errorf("tool result %q does not carry the response body (external_id); loopback body round-trip broken", text)
	}
}

func TestE2E_GetProfileHandlerErrorSurfaces(t *testing.T) {
	deps := newTestServer(t)
	// A handler-returned connect error travels a different recorder path than a
	// pre-handler validation rejection (populated error body + connect status
	// mapping); it must still surface as a model-readable tool error, not a
	// swallowed success or a dropped connection.
	deps.profiles.getErr = connect.NewError(connect.CodeNotFound, errors.New("profile not found"))
	session, ctx := connectMCP(t, deps.server.URL+"/mcp", pogrpc.HeaderAPIKey, testPrivateKey)

	res, err := session.CallTool(ctx, &gomcp.CallToolParams{
		Name:      "get_profile",
		Arguments: map[string]any{"id": "user-404"},
	})
	if err != nil {
		t.Fatalf("CallTool returned a protocol error, want a tool error result: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected the handler error surfaced as a tool error, got success: %s", toolText(res))
	}
	if text := toolText(res); !strings.Contains(text, "profile not found") {
		t.Errorf("tool error text %q does not contain the handler error message", text)
	}
}

func TestE2E_QueryInsightsValidationRejected(t *testing.T) {
	deps := newTestServer(t)
	session, ctx := connectMCP(t, deps.server.URL+"/mcp", pogrpc.HeaderAPIKey, testPrivateKey)

	// Empty args violate the required spec/time_range/granularity constraints.
	res, err := session.CallTool(ctx, &gomcp.CallToolParams{
		Name:      "query_insights",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool returned a protocol error, want a tool error result: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected a validation tool error, got success: %s", toolText(res))
	}
	if text := toolText(res); !strings.Contains(text, "spec") {
		t.Errorf("validation error text %q does not mention the offending field", text)
	}
	if deps.insights.queryCalls != 0 {
		t.Errorf("handler was invoked %d times; validation must reject before the handler", deps.insights.queryCalls)
	}
}

func TestE2E_BearerAuthSucceeds(t *testing.T) {
	deps := newTestServer(t)
	session, ctx := connectMCP(t, deps.server.URL+"/mcp", "Authorization", "Bearer "+testPrivateKey)

	res, err := session.CallTool(ctx, &gomcp.CallToolParams{
		Name:      "get_profile",
		Arguments: map[string]any{"id": "user-xyz"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success over Bearer auth, got: %s", toolText(res))
	}
	if deps.profiles.gotID != "user-xyz" {
		t.Errorf("handler saw id %q, want %q", deps.profiles.gotID, "user-xyz")
	}
}

func TestE2E_UnauthorizedRawPOST(t *testing.T) {
	deps := newTestServer(t)

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	cases := []struct {
		name   string
		header string
		value  string
	}{
		{"no key", "", ""},
		{"public key", pogrpc.HeaderAPIKey, "pub_something"},
		{"unknown private key", pogrpc.HeaderAPIKey, "prv_unknown"},
		{"non-prv bearer", "Authorization", "Bearer pub_something"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, deps.server.URL+"/mcp", strings.NewReader(body))
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			if tc.header != "" {
				req.Header.Set(tc.header, tc.value)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
		})
	}
}

// ---------- unit ----------

type recordingServer struct{ names []string }

func (r *recordingServer) AddTool(tool mcpruntime.Tool, _ mcpruntime.ToolHandler) {
	r.names = append(r.names, tool.Name)
}

func TestRegisterToolsPinsCuratedSurface(t *testing.T) {
	rec := &recordingServer{}
	srv := newRenamer(rec)
	// Registration only builds tool descriptors; the loopback is never dialed.
	registerTools(srv, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	if err := srv.checkComplete(); err != nil {
		t.Fatalf("rename table drifted from generated tools: %v", err)
	}

	got := append([]string(nil), rec.names...)
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(curatedToolNames, ",") {
		t.Errorf("registered tools =\n  %v\nwant\n  %v", got, curatedToolNames)
	}
}

// TestRenamerRejectsUnnamedTool exercises the FAILURE direction of the drift
// guard: a generated tool with no curated name must make checkComplete error
// (and must NOT reach the inner server). This is the case a codegen change that
// adds a new shared RPC would hit — it should fail startup, not ship unnamed.
func TestRenamerRejectsUnnamedTool(t *testing.T) {
	rec := &recordingServer{}
	r := newRenamer(rec)
	r.AddTool(mcpruntime.Tool{Name: "shared_bogus_v1_BogusService_Frobnicate"}, nil)

	err := r.checkComplete()
	if err == nil {
		t.Fatal("checkComplete = nil, want an error for a tool with no curated name")
	}
	if !strings.Contains(err.Error(), "no curated name") {
		t.Errorf("error = %q, want it to mention %q", err.Error(), "no curated name")
	}
	if len(rec.names) != 0 {
		t.Errorf("unnamed tool was forwarded to the inner server (%v); it must be dropped", rec.names)
	}
}

// TestRenamerRejectsStaleEntry exercises the other FAILURE direction: a rename
// table entry that no generated tool consumed (a stale entry left behind after
// an RPC is removed upstream) must make checkComplete error. Registering only
// one of the curated tools leaves the rest stale.
func TestRenamerRejectsStaleEntry(t *testing.T) {
	r := newRenamer(&recordingServer{})
	r.AddTool(insightsv1mcp.InsightsService_QueryTool, nil)

	err := r.checkComplete()
	if err == nil {
		t.Fatal("checkComplete = nil, want an error for unconsumed (stale) rename entries")
	}
	if !strings.Contains(err.Error(), "stale") {
		t.Errorf("error = %q, want it to mention %q", err.Error(), "stale")
	}
}

func TestKeepProfileToolExcludesErasure(t *testing.T) {
	// The two GDPR erasure tools must be filtered; every curated profiles read
	// must pass. Referencing the generated Tool vars keeps this in lockstep with
	// codegen.
	excluded := map[string]bool{
		"shared_profiles_v1_ProfilesService_Delete":            true,
		"shared_profiles_v1_ProfilesService_DeleteDataSubject": true,
	}
	for name, wantExcluded := range excluded {
		if keepProfileTool(name) == wantExcluded {
			t.Errorf("keepProfileTool(%q) = %v, want %v", name, keepProfileTool(name), !wantExcluded)
		}
	}
	if !keepProfileTool("shared_profiles_v1_ProfilesService_Get") {
		t.Error("keepProfileTool excluded a read RPC")
	}
}

func TestWithAPIKeyPassthrough(t *testing.T) {
	cases := []struct {
		name       string
		apiKey     string // x-api-key header
		authHeader string // Authorization header
		wantKey    string // expected x-api-key seen downstream + stashed
	}{
		{"x-api-key passes through", testPrivateKey, "", testPrivateKey},
		{"bearer prv normalized", "", "Bearer " + testPrivateKey, testPrivateKey},
		{"lowercase bearer prv normalized", "", "bearer " + testPrivateKey, testPrivateKey},
		{"uppercase BEARER prv normalized", "", "BEARER " + testPrivateKey, testPrivateKey},
		{"bearer pub not normalized", "", "Bearer pub_x", ""},
		{"bearer non-key not normalized", "", "Bearer abc123", ""},
		{"bare bearer scheme not normalized", "", "Bearer", ""},
		{"no credential", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotHeader, gotCtx string
			next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				gotHeader = r.Header.Get(pogrpc.HeaderAPIKey)
				gotCtx = apiKeyFromContext(r.Context())
			})
			req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			if tc.apiKey != "" {
				req.Header.Set(pogrpc.HeaderAPIKey, tc.apiKey)
			}
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}
			withAPIKeyPassthrough(next).ServeHTTP(httptest.NewRecorder(), req)

			if gotHeader != tc.wantKey {
				t.Errorf("downstream x-api-key = %q, want %q", gotHeader, tc.wantKey)
			}
			if gotCtx != tc.wantKey {
				t.Errorf("stashed ctx key = %q, want %q", gotCtx, tc.wantKey)
			}
		})
	}
}

func TestLoopbackInjectsAPIKey(t *testing.T) {
	var gotKey string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get(pogrpc.HeaderAPIKey)
		w.WriteHeader(http.StatusNoContent)
	})
	lb := &loopbackClient{handler: h}

	req, err := http.NewRequest(http.MethodPost, loopbackBaseURL+"/shared.x/Y", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req = req.WithContext(withAPIKey(context.Background(), "prv_injected"))

	resp, err := lb.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if gotKey != "prv_injected" {
		t.Errorf("inner handler saw x-api-key %q, want %q", gotKey, "prv_injected")
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
}
