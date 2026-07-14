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
	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
	pogrpc "github.com/pug-sh/pug/internal/app/server/rpc"
	"github.com/pug-sh/pug/internal/correlation"
	coreauth "github.com/pug-sh/pug/internal/core/auth"
	"github.com/pug-sh/pug/internal/core/authz"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	"github.com/pug-sh/pug/internal/gen/proto/shared/activity/v1/activityv1connect"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1/insightsv1connect"
	insightsv1mcp "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1/insightsv1mcp"
	profilesv1 "github.com/pug-sh/pug/internal/gen/proto/shared/profiles/v1"
	"github.com/pug-sh/pug/internal/gen/proto/shared/profiles/v1/profilesv1connect"
	profilesv1mcp "github.com/pug-sh/pug/internal/gen/proto/shared/profiles/v1/profilesv1mcp"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	mcpruntime "github.com/redpanda-data/protoc-gen-go-mcp/pkg/runtime"
	"google.golang.org/protobuf/proto"
)

const testPrivateKey = "prv_testkey"

// testJWTKey signs the dashboard tokens the shared services accept, so the /mcp
// rejection tests can present a genuinely valid dashboard credential rather than
// a malformed one.
var testJWTKey = []byte("test-jwt-key")

// signDashboardJWT mints a real dashboard access token — the exact shape
// WithJWTAuth accepts. /mcp must refuse it: MCP clients hold a static credential,
// so a 1h-expiry access token is useless there, and admitting JWTs would widen the
// endpoint from project-scoped to customer-scoped.
func signDashboardJWT(t *testing.T) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "cust-mcp",
		"aud": coreauth.Audience,
		"iss": coreauth.Issuer,
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	signed, err := token.SignedString(testJWTKey)
	if err != nil {
		t.Fatalf("sign dashboard JWT: %v", err)
	}
	return signed
}

// curatedToolNames is the pinned public contract: exactly the tools /mcp exposes.
// Kept as a literal (deliberately NOT derived from toolPolicy) so an accidental
// edit to the policy table is caught here — a derived list would pass tautologically.
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
	gotID       string
	sawProject  bool
	sawDeadline bool
	getResp     *profilesv1.GetResponse // if set, returned from Get
	getErr      error                   // if set, returned from Get (takes precedence)
	getPanic    bool                    // if set, Get panics (takes precedence over all)
}

func (s *stubProfiles) Get(ctx context.Context, req *connect.Request[profilesv1.GetRequest]) (*connect.Response[profilesv1.GetResponse], error) {
	s.gotID = req.Msg.GetId()
	if p, err := pogrpc.MustGetPrincipalWithProject(ctx); err == nil && p.Project != nil {
		s.sawProject = true
	}
	_, s.sawDeadline = ctx.Deadline()
	if s.getPanic {
		// Stands in for a latent nil deref / nil-map write / out-of-range index in a
		// real handler — the class of bug an LLM's arbitrary-but-schema-valid arguments
		// are most likely to find. Reached through /mcp it unwinds on a go-sdk jsonrpc2
		// goroutine that net/http's per-connection recover does not cover, so without
		// containment it kills the process.
		panic("simulated handler bug")
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
	// handler sees an authenticated Principal; WithRecover mirrors production so
	// TestE2E_HandlerPanicSurfacesAsToolError exercises the real panic containment
	// rather than a harness-only approximation.
	handlerOpts := connect.WithHandlerOptions(
		connect.WithInterceptors(
			pogrpc.CorrelationInterceptor(),
			pogrpc.LoggingInterceptor(),
			pogrpc.ErrorInterceptor(),
			validate.NewInterceptor(validate.WithoutErrorDetails()),
			pogrpc.PrincipalInterceptor(),
			pogrpc.AuthzInterceptor(authorizer, stubRoleLookup{}),
		),
		connect.WithRecover(pogrpc.RecoverHandlerPanic),
	)

	mux := http.NewServeMux()
	// The shared services keep dual auth (private key OR dashboard JWT), exactly as
	// in production — which is what gives the /mcp JWT-rejection test its teeth: the
	// same JWT that these handlers would accept must still be refused at /mcp.
	sharedMW := authn.NewMiddleware(pogrpc.WithDualAuth(testJWTKey, nil, repo))

	ip, ih := insightsv1connect.NewInsightsServiceHandler(insights, handlerOpts)
	mux.Handle(ip, sharedMW.Wrap(ih))
	ap, ah := activityv1connect.NewActivityServiceHandler(&stubActivity{}, handlerOpts)
	mux.Handle(ap, sharedMW.Wrap(ah))
	pp, ph := profilesv1connect.NewProfilesServiceHandler(profiles, handlerOpts)
	mux.Handle(pp, sharedMW.Wrap(ph))

	// Identical to the production call in server.start. Mount builds the handler and
	// the private-key-only auth boundary itself, so the harness cannot wire /mcp
	// differently than production does — the bug that let an earlier version of this
	// suite stay green with the endpoint pointed at WithDualAuth.
	if err := Mount(mux, mux, repo); err != nil {
		t.Fatalf("Mount: %v", err)
	}

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

// TestE2E_HandlerPanicSurfacesAsToolError is the regression guard for the
// loopback's worst failure mode. A handler panic on the ordinary Connect path is
// contained by net/http's per-connection recover, but a tool call runs the
// handler on a go-sdk jsonrpc2 goroutine that recover never reaches — so an
// escaping panic took down the whole process (every tenant), remotely triggerable
// by any private-key holder. The panic must surface as a tool error and the
// process must live.
func TestE2E_HandlerPanicSurfacesAsToolError(t *testing.T) {
	deps := newTestServer(t)
	deps.profiles.getPanic = true
	session, ctx := connectMCP(t, deps.server.URL+"/mcp", pogrpc.HeaderAPIKey, testPrivateKey)

	res, err := session.CallTool(ctx, &gomcp.CallToolParams{
		Name:      "get_profile",
		Arguments: map[string]any{"id": "user-boom"},
	})
	if err != nil {
		t.Fatalf("CallTool returned a protocol error, want a tool error result: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected the handler panic surfaced as a tool error, got success: %s", toolText(res))
	}
	// The client must not be handed the panic value or a stack trace.
	if text := toolText(res); strings.Contains(text, "simulated handler bug") {
		t.Errorf("tool error text %q leaks the panic value; it must be a generic internal error", text)
	}
}

// TestLoopbackRecoversPanicOutsideConnect covers the second layer. connect's
// WithRecover only contains panics inside the Connect handler chain; a panic in
// the authn middleware or in mux routing unwinds past it and still reaches the
// jsonrpc2 goroutine. Do is the backstop that must catch anything, so it is
// exercised here against a raw panicking handler with no Connect chain at all.
//
// It is also the only place the sanitisation on this layer can be pinned.
// TestE2E_HandlerPanicSurfacesAsToolError panics *inside* a handler, so
// RecoverHandlerPanic contains it first and the recover here never runs — that test
// guards a path which is already safe. Only a panic from outside the Connect chain
// reaches the error Do builds, and runtime.HandleError renders that error straight
// into the tool text the model reads.
func TestLoopbackRecoversPanicOutsideConnect(t *testing.T) {
	lb := &loopbackClient{handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom outside the connect chain")
	})}

	req, err := http.NewRequestWithContext(
		correlation.WithID(withAPIKey(context.Background(), testPrivateKey), "test-correlation-id"),
		http.MethodPost, loopbackBaseURL+"/shared.x/Y", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp, doErr := lb.Do(req) // must not panic
	if doErr == nil {
		t.Fatal("Do returned nil error after a handler panic; the panic must be contained and reported")
	}
	if resp != nil {
		t.Errorf("Do returned a response alongside the panic error: %v", resp)
	}
	if strings.Contains(doErr.Error(), "boom outside the connect chain") {
		t.Errorf("error = %q leaks the panic value; it must be a generic internal error", doErr.Error())
	}
	// The id is what makes the sanitised error actionable: without it nothing ties the
	// model's "internal error" back to the log line holding the panic value and stack.
	if !strings.Contains(doErr.Error(), "test-correlation-id") {
		t.Errorf("error = %q, want it to carry the correlation id", doErr.Error())
	}
}

// TestPanicToolError pins the fallback branch the loopback test cannot reach: with no
// correlation id in context the message must degrade cleanly, not trail an empty
// "(correlation id: )".
func TestPanicToolError(t *testing.T) {
	got := panicToolError(context.Background()).Error()
	if !strings.Contains(got, "internal error") {
		t.Errorf("error = %q, want a generic internal error", got)
	}
	if strings.Contains(got, "correlation id") {
		t.Errorf("error = %q, want no correlation-id clause when the context carries none", got)
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
		// A valid dashboard JWT — one the shared Connect services would accept —
		// must still be refused at /mcp. Mount builds WithPrivateKeyAuth itself, so
		// there is no wiring in which this endpoint admits a JWT.
		{"valid dashboard jwt", "Authorization", "Bearer " + signDashboardJWT(t)},
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

// TestE2E_TrailingSlashServed pins both spellings of the endpoint. Go 1.22's
// ServeMux exact-matches a pattern with no trailing slash, and the subtree
// redirect only fires the other way, so "/mcp/" would 404 — an opaque failure for
// anyone who pastes the URL with a slash or sits behind an ingress that appends
// one. MCP clients POST byte-for-byte what they are configured with.
func TestE2E_TrailingSlashServed(t *testing.T) {
	deps := newTestServer(t)
	session, ctx := connectMCP(t, deps.server.URL+"/mcp/", pogrpc.HeaderAPIKey, testPrivateKey)

	res, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools over /mcp/: %v", err)
	}
	if len(res.Tools) != len(curatedToolNames) {
		t.Errorf("tools over /mcp/ = %d, want %d", len(res.Tools), len(curatedToolNames))
	}
}

// ---------- unit ----------

type recordingServer struct{ names []string }

func (r *recordingServer) AddTool(tool mcpruntime.Tool, _ mcpruntime.ToolHandler) {
	r.names = append(r.names, tool.Name)
}

func TestRegisterToolsPinsCuratedSurface(t *testing.T) {
	rec := &recordingServer{}
	// Registration only builds tool descriptors; the loopback is never dialed.
	err := registerRenamed(rec, func(srv mcpruntime.MCPServer) {
		registerTools(srv, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	})
	if err != nil {
		t.Fatalf("tool policy drifted from generated tools: %v", err)
	}

	got := append([]string(nil), rec.names...)
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(curatedToolNames, ",") {
		t.Errorf("registered tools =\n  %v\nwant\n  %v", got, curatedToolNames)
	}
}

// TestRenamerRejectsUnknownTool exercises the FAILURE direction of the drift
// guard: a generated tool with no policy row must make registration error (and
// must NOT reach the inner server). This is the case a codegen change that adds a
// new shared RPC hits — it must fail startup, not ship unnamed.
func TestRenamerRejectsUnknownTool(t *testing.T) {
	rec := &recordingServer{}
	err := registerRenamed(rec, func(srv mcpruntime.MCPServer) {
		srv.AddTool(mcpruntime.Tool{Name: "shared_bogus_v1_BogusService_Frobnicate"}, nil)
	})

	if err == nil {
		t.Fatal("registerRenamed = nil, want an error for a tool with no policy entry")
	}
	if !strings.Contains(err.Error(), "no entry in the tool policy table") {
		t.Errorf("error = %q, want it to mention the missing policy entry", err.Error())
	}
	if len(rec.names) != 0 {
		t.Errorf("unknown tool was forwarded to the inner server (%v); it must be dropped", rec.names)
	}
}

// TestRenamerRejectsStaleEntry exercises the other FAILURE direction: an exposed
// policy row that no generated tool consumed (left behind after an RPC is removed
// upstream) must error. Registering only one tool leaves the rest stale.
func TestRenamerRejectsStaleEntry(t *testing.T) {
	err := registerRenamed(&recordingServer{}, func(srv mcpruntime.MCPServer) {
		srv.AddTool(insightsv1mcp.InsightsService_QueryTool, nil)
	})

	if err == nil {
		t.Fatal("registerRenamed = nil, want an error for unconsumed (stale) policy entries")
	}
	if !strings.Contains(err.Error(), "stale") {
		t.Errorf("error = %q, want it to mention %q", err.Error(), "stale")
	}
}

// TestRenamerRejectsHiddenToolReachingRegistration is the belt-and-braces guard.
// keepTool already withholds hidden tools, so this state is unreachable in
// production — which is exactly why it must fail loudly if it ever happens: a
// regressed filter must not silently hand GDPR erasure to an LLM agent.
func TestRenamerRejectsHiddenToolReachingRegistration(t *testing.T) {
	rec := &recordingServer{}
	err := registerRenamed(rec, func(srv mcpruntime.MCPServer) {
		srv.AddTool(profilesv1mcp.ProfilesService_DeleteTool, nil) // bypasses keepTool
	})

	if err == nil {
		t.Fatal("registerRenamed = nil, want an error for a hidden tool reaching registration")
	}
	if !strings.Contains(err.Error(), "hidden tool") {
		t.Errorf("error = %q, want it to mention the hidden tool", err.Error())
	}
	if len(rec.names) != 0 {
		t.Errorf("hidden tool %v was forwarded to the inner server; erasure must never be exposed", rec.names)
	}
}

// TestRenamerRejectsCuratedNameCollision guards a failure the old rename table was
// blind to: two generated tools mapping to one curated name. checkComplete saw no
// stale entry and no unnamed tool, while the go-sdk's feature set silently REPLACED
// the first registration with the second — quietly dropping a tool from the surface.
func TestRenamerRejectsCuratedNameCollision(t *testing.T) {
	rec := &recordingServer{}
	// Two real generated tools, both forced through the same curated name.
	err := registerRenamed(rec, func(srv mcpruntime.MCPServer) {
		r, ok := srv.(*renamer)
		if !ok {
			t.Fatalf("registerRenamed passed a %T, want *renamer", srv)
		}
		r.claimed["query_insights"] = "shared_insights_v1_InsightsService_Impostor"
		srv.AddTool(insightsv1mcp.InsightsService_QueryTool, nil)
	})

	if err == nil {
		t.Fatal("registerRenamed = nil, want an error for a duplicate curated name")
	}
	if !strings.Contains(err.Error(), "claimed by both") {
		t.Errorf("error = %q, want it to report the collision", err.Error())
	}
}

func TestKeepToolWithholdsErasureAndSegmentUsers(t *testing.T) {
	// The irreversible erasure RPCs and the WIP SegmentUsers must be withheld; every
	// curated read must pass. Referencing the generated Tool vars (rather than string
	// literals) keeps this in lockstep with codegen at compile time.
	for _, tl := range []mcpruntime.Tool{
		profilesv1mcp.ProfilesService_DeleteTool,
		profilesv1mcp.ProfilesService_DeleteDataSubjectTool,
		insightsv1mcp.InsightsService_SegmentUsersTool,
	} {
		if keepTool(tl.Name) {
			t.Errorf("keepTool(%q) = true, want it withheld", tl.Name)
		}
	}
	for _, tl := range []mcpruntime.Tool{
		profilesv1mcp.ProfilesService_GetTool,
		insightsv1mcp.InsightsService_QueryTool,
	} {
		if !keepTool(tl.Name) {
			t.Errorf("keepTool(%q) = false, want it exposed", tl.Name)
		}
	}
	// An unrecognised tool must PASS the filter so the renamer rejects it and startup
	// fails. Silently dropping it would ship a hole in the tool surface.
	if !keepTool("shared_bogus_v1_BogusService_Frobnicate") {
		t.Error("keepTool dropped an unknown tool; it must pass so the renamer can reject it")
	}
}

// TestInstructionsMatchToolPolicy pins the server's LLM-facing instructions against
// the tool surface. instructions is prose and has no compile-time link to the
// policy table, so without this a curated rename leaves the server telling agents
// to call a tool that no longer exists, and a newly exposed tool goes unmentioned.
//
// Only the exposed direction is checkable: a hidden tool has no curated name, so
// there is no string whose absence could be asserted.
func TestInstructionsMatchToolPolicy(t *testing.T) {
	for generated, d := range toolPolicy {
		if !d.exposed() {
			continue
		}
		if !strings.Contains(instructions, d.curated) {
			t.Errorf("instructions never mention exposed tool %q (%s); an agent is never told it exists",
				d.curated, generated)
		}
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
		// Precedence matters for more than tidiness: NormalizeBearerAPIKey MUTATES the
		// request header, so if a Bearer could overwrite an explicit x-api-key, anything
		// able to inject an Authorization header (a proxy, a misconfigured gateway) could
		// swap the caller onto another project's key. x-api-key must win.
		{"x-api-key wins over a conflicting bearer", testPrivateKey, "Bearer prv_otherproject", testPrivateKey},
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

// TestE2E_ToolCallContextIsBounded pins the deadline the loopback imposes. The
// go-sdk runs tool handlers on a jsonrpc2 context with cancellation detached
// (Done and Deadline are nil/zero, though Values still resolve), so a client that
// hangs up mid-query cancels nothing: without a deadline of our own, a wide
// query_insights keeps a ClickHouse query running to completion with nobody
// waiting for it. Every insight and profile handler threads ctx into its query, so
// a deadline here is what bounds them.
func TestE2E_ToolCallContextIsBounded(t *testing.T) {
	deps := newTestServer(t)
	session, ctx := connectMCP(t, deps.server.URL+"/mcp", pogrpc.HeaderAPIKey, testPrivateKey)

	if _, err := session.CallTool(ctx, &gomcp.CallToolParams{
		Name:      "get_profile",
		Arguments: map[string]any{"id": "user-123"},
	}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	if !deps.profiles.sawDeadline {
		t.Error("inner handler ran with no deadline; a tool call must be bounded or a runaway query has nothing to stop it")
	}
}

// TestLoopbackFailsLoudWithoutAPIKey covers the wiring-bug path. /mcp is
// private-key-only and withAPIKeyPassthrough stashes the key outside the authn
// boundary, so every request that reaches a tool handler provably carries one —
// key == "" is unreachable in production and therefore means the endpoint was
// mounted wrong. It must say so, not silently omit the header and let the inner
// auth answer with a 401 that (being outside the interceptor chain) logs nothing
// and tells the operator their credential is bad.
func TestLoopbackFailsLoudWithoutAPIKey(t *testing.T) {
	lb := &loopbackClient{handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("inner handler ran without an API key; the loopback must refuse first")
	})}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, loopbackBaseURL+"/shared.x/Y", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	if _, doErr := lb.Do(req); doErr == nil {
		t.Fatal("Do returned nil error with no API key in context; a missing key is a wiring bug and must not degrade into a mystery 401")
	}
}

func TestResponseRecorder(t *testing.T) {
	t.Run("defaults to 200 when WriteHeader is never called", func(t *testing.T) {
		rec := newResponseRecorder()
		if _, err := rec.Write([]byte("body")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if got := rec.result(nil).StatusCode; got != http.StatusOK {
			t.Errorf("StatusCode = %d, want 200", got)
		}
	})

	t.Run("first WriteHeader wins", func(t *testing.T) {
		rec := newResponseRecorder()
		rec.WriteHeader(http.StatusNotFound)
		rec.WriteHeader(http.StatusInternalServerError)
		if got := rec.result(nil).StatusCode; got != http.StatusNotFound {
			t.Errorf("StatusCode = %d, want the first status (404)", got)
		}
	})

	t.Run("a late WriteHeader after a body write is ignored", func(t *testing.T) {
		rec := newResponseRecorder()
		if _, err := rec.Write([]byte("body")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		rec.WriteHeader(http.StatusInternalServerError)
		if got := rec.result(nil).StatusCode; got != http.StatusOK {
			t.Errorf("StatusCode = %d; the implicit 200 committed by Write must stand", got)
		}
	})

	t.Run("Status carries the numeric code", func(t *testing.T) {
		// connect surfaces Response.Status verbatim as the error message when the body
		// is not connect-wire JSON (an inner-mux 404, say), and that string is all the
		// operator and the model ever see on a path that logs nothing.
		rec := newResponseRecorder()
		rec.WriteHeader(http.StatusNotFound)
		if got := rec.result(nil).Status; got != "404 Not Found" {
			t.Errorf("Status = %q, want %q", got, "404 Not Found")
		}
	})

	t.Run("ContentLength reports the buffered body", func(t *testing.T) {
		rec := newResponseRecorder()
		if _, err := rec.Write([]byte("hello")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if got := rec.result(nil).ContentLength; got != int64(len("hello")) {
			t.Errorf("ContentLength = %d, want %d", got, len("hello"))
		}
	})

	t.Run("the response does not alias the recorder's header map", func(t *testing.T) {
		// net/http snapshots headers when the status commits. A live map would let a
		// header set after the fact appear on an already-returned response.
		rec := newResponseRecorder()
		rec.Header().Set("X-Before", "1")
		resp := rec.result(nil)
		rec.Header().Set("X-After", "2")

		if resp.Header.Get("X-Before") != "1" {
			t.Error("committed header was lost")
		}
		if resp.Header.Get("X-After") != "" {
			t.Error("a header set after result() leaked into the returned response; it must be a snapshot")
		}
	})
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
