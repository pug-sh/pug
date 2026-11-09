package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/authn"
	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"
	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/auth"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/xid"
	"golang.org/x/oauth2"
	"google.golang.org/protobuf/proto"

	pogrpc "github.com/pug-sh/pug/internal/app/server/rpc"
	mcpgrantsrpc "github.com/pug-sh/pug/internal/app/server/rpc/dashboard/mcpgrants"
	sharedprojectsrpc "github.com/pug-sh/pug/internal/app/server/rpc/shared/projects"
	"github.com/pug-sh/pug/internal/cimd"
	"github.com/pug-sh/pug/internal/core/authz"
	"github.com/pug-sh/pug/internal/core/mcpgrants"
	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	"github.com/pug-sh/pug/internal/deps/postgres"
	mcpgrantsv1 "github.com/pug-sh/pug/internal/gen/proto/dashboard/mcpgrants/v1"
	"github.com/pug-sh/pug/internal/gen/proto/dashboard/mcpgrants/v1/mcpgrantsv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/shared/profiles/v1/profilesv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/shared/projects/v1/sharedprojectsv1connect"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

const (
	testIssuer      = "https://api.pugs.dev"
	testConsentPage = "https://app.pug.sh/oauth/authorize"
	initializeBody  = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`
)

// newOAuthTestServer has OAuth on over no database, which serves everything short
// of looking up a code or a token.
func newOAuthTestServer(t *testing.T) testDeps {
	t.Helper()
	return newTestServerWith(t, OAuth{Grants: mcpgrants.NewService(nil, nil, testIssuer), AuthorizationEndpoint: testConsentPage})
}

// postMCP sends an initialize with at most one credential header, and returns the
// response with its body drained.
func postMCP(t *testing.T, endpoint, header, value string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(initializeBody))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if header != "" {
		req.Header.Set(header, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", endpoint, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp
}

func TestOAuthMetadata(t *testing.T) {
	deps := newOAuthTestServer(t)
	protected := func(resource string) map[string]any {
		return map[string]any{
			"resource":                 resource,
			"authorization_servers":    []any{testIssuer},
			"scopes_supported":         []any{"read"},
			"bearer_methods_supported": []any{"header"},
			"resource_name":            "pug",
		}
	}
	for path, want := range map[string]map[string]any{
		"/.well-known/oauth-protected-resource":      protected(testIssuer + "/mcp"),
		"/.well-known/oauth-protected-resource/mcp":  protected(testIssuer + "/mcp"),
		"/.well-known/oauth-protected-resource/mcp/": protected(testIssuer + "/mcp/"),
		// Claude and ChatGPT use CIMD only with client_id_metadata_document_supported
		// and "none", and ChatGPT keeps a stable client_id only with iss supported.
		"/.well-known/oauth-authorization-server": {
			"issuer":                                         testIssuer,
			"authorization_endpoint":                         testConsentPage,
			"token_endpoint":                                 testIssuer + "/oauth/token",
			"scopes_supported":                               []any{"read"},
			"response_types_supported":                       []any{"code"},
			"grant_types_supported":                          []any{"authorization_code", "refresh_token"},
			"code_challenge_methods_supported":               []any{"S256"},
			"token_endpoint_auth_methods_supported":          []any{"none"},
			"client_id_metadata_document_supported":          true,
			"authorization_response_iss_parameter_supported": true,
		},
	} {
		resp, err := http.Get(deps.server.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		var got map[string]any
		err = json.NewDecoder(resp.Body).Decode(&got)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || !reflect.DeepEqual(got, want) {
			t.Errorf("GET %s = %d %v (%v), want %v", path, resp.StatusCode, got, err, want)
		}
		if origin := resp.Header.Get("Access-Control-Allow-Origin"); origin != "*" {
			t.Errorf("GET %s: Access-Control-Allow-Origin = %q, want *", path, origin)
		}
	}
}

// Browser-based MCP clients send a preflight first.
func TestOAuthRoutesAnswerPreflights(t *testing.T) {
	deps := newOAuthTestServer(t)
	for path, method := range map[string]string{
		"/.well-known/oauth-protected-resource/mcp": http.MethodGet,
		"/.well-known/oauth-authorization-server":   http.MethodGet,
		"/oauth/token": http.MethodPost,
	} {
		req, err := http.NewRequest(http.MethodOptions, deps.server.URL+path, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Origin", "https://inspector.example")
		req.Header.Set("Access-Control-Request-Method", method)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("OPTIONS %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Access-Control-Allow-Origin") != "*" ||
			!strings.Contains(resp.Header.Get("Access-Control-Allow-Methods"), method) {
			t.Errorf("OPTIONS %s = %d %v, want 204 allowing %s from any origin", path, resp.StatusCode, resp.Header, method)
		}
	}

	resp, err := http.Get(deps.server.URL + "/oauth/token")
	if err != nil {
		t.Fatalf("GET /oauth/token: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /oauth/token = %d, want 405", resp.StatusCode)
	}
}

func TestOAuthOffServesNoOAuth(t *testing.T) {
	deps := newTestServer(t)
	for _, path := range []string{"/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-authorization-server", "/oauth/token"} {
		resp, err := http.Get(deps.server.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 while OAuth is off", path, resp.StatusCode)
		}
	}
	if resp := postMCP(t, deps.server.URL+"/mcp", "", ""); resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") != "" {
		t.Errorf("POST /mcp = %d %q, want a plain 401", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
}

// Half an OAuth config is a wiring bug: it would publish an empty
// authorization_endpoint, or leave OAuth silently off.
func TestMountRefusesHalfOAuth(t *testing.T) {
	for _, oauth := range []OAuth{
		{Grants: mcpgrants.NewService(nil, nil, testIssuer)},
		{AuthorizationEndpoint: testConsentPage},
	} {
		if err := Mount(http.NewServeMux(), http.NewServeMux(), &stubKeyLookup{}, oauth); err == nil {
			t.Errorf("Mount(%+v) = nil, want an error", oauth)
		}
	}
}

// Clients start signing in only on a 401 that carries this challenge.
func TestOAuthChallenge(t *testing.T) {
	deps := newOAuthTestServer(t)
	const metadata = testIssuer + "/.well-known/oauth-protected-resource/mcp"
	refused := `Bearer error="invalid_token", resource_metadata="` + metadata + `", scope="read"`
	for _, tc := range []struct{ name, path, header, value, want string }{
		{"no credential", "/mcp", "", "", `Bearer resource_metadata="` + metadata + `", scope="read"`},
		{"no credential at /mcp/", "/mcp/", "", "", `Bearer resource_metadata="` + metadata + `/", scope="read"`},
		{"unknown private key", "/mcp", pogrpc.HeaderAPIKey, "prv_unknown", refused},
		{"public key", "/mcp", pogrpc.HeaderAPIKey, "pub_something", refused},
		{"dashboard jwt", "/mcp", "Authorization", "Bearer " + signDashboardJWT(t, jwt.MapClaims{"sub": "cust-mcp"}), refused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := postMCP(t, deps.server.URL+tc.path, tc.header, tc.value)
			if got := resp.Header.Get("WWW-Authenticate"); resp.StatusCode != http.StatusUnauthorized || got != tc.want {
				t.Errorf("POST %s = %d %q, want 401 %q", tc.path, resp.StatusCode, got, tc.want)
			}
		})
	}

	session, ctx := connectMCP(t, deps.server.URL+"/mcp", pogrpc.HeaderAPIKey, testPrivateKey)
	if _, err := session.ListTools(ctx, nil); err != nil {
		t.Errorf("a private key no longer works with OAuth on: %v", err)
	}
}

func TestTokenEndpointRefusals(t *testing.T) {
	deps := newOAuthTestServer(t)
	exchange := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {"a-code"},
		"client_id":     {"https://claude.ai/oauth/mcp-oauth-client-metadata"},
		"redirect_uri":  {"https://claude.ai/api/mcp/auth_callback"},
		"code_verifier": {strings.Repeat("v", 43)},
	}
	with := func(key, value string) url.Values {
		v := maps.Clone(exchange)
		v.Set(key, value)
		return v
	}
	for _, tc := range []struct {
		name   string
		query  string
		form   url.Values
		basic  bool
		status int
		want   string
	}{
		// Refused before the code is looked up, which would use it up.
		{name: "basic auth", form: exchange, basic: true, status: http.StatusUnauthorized, want: "invalid_client"},
		{name: "client_secret", form: with("client_secret", "s"), status: http.StatusUnauthorized, want: "invalid_client"},
		{name: "no grant_type", form: url.Values{}, status: http.StatusBadRequest, want: "invalid_request"},
		{name: "query string ignored", query: "?" + exchange.Encode(), form: url.Values{}, status: http.StatusBadRequest, want: "invalid_request"},
		{name: "password grant", form: url.Values{"grant_type": {"password"}}, status: http.StatusBadRequest, want: "unsupported_grant_type"},
		{name: "no code", form: with("code", ""), status: http.StatusBadRequest, want: "invalid_request"},
		{name: "another resource", form: with("resource", "https://evil.example/mcp"), status: http.StatusBadRequest, want: "invalid_target"},
		{name: "no refresh token", form: url.Values{"grant_type": {"refresh_token"}}, status: http.StatusBadRequest, want: "invalid_request"},
		{name: "oversized", form: with("pad", strings.Repeat("x", maxTokenRequestBytes)), status: http.StatusBadRequest, want: "invalid_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, deps.server.URL+"/oauth/token"+tc.query, strings.NewReader(tc.form.Encode()))
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if tc.basic {
				req.SetBasicAuth(exchange.Get("client_id"), "")
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			var body map[string]string
			err = json.NewDecoder(resp.Body).Decode(&body)
			_ = resp.Body.Close()
			if err != nil || resp.StatusCode != tc.status || body["error"] != tc.want {
				t.Errorf("= %d %v (%v), want %d %s", resp.StatusCode, body, err, tc.status, tc.want)
			}
			if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
				t.Errorf("headers = %v, want no-store to any origin", resp.Header)
			}
			if tc.status == http.StatusUnauthorized && resp.Header.Get("WWW-Authenticate") != "Basic" {
				t.Errorf("WWW-Authenticate = %q, want Basic", resp.Header.Get("WWW-Authenticate"))
			}
		})
	}
}

// A database failure is a 503 at /oauth/token and at /mcp, with no challenge, so
// the client retries rather than signing in again.
func TestDatabaseFailureIsUnavailable(t *testing.T) {
	pool, err := pgxpool.New(t.Context(), "postgres://127.0.0.1:1/pug")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	pool.Close()
	deps := newTestServerWith(t, OAuth{Grants: mcpgrants.NewService(pool, nil, testIssuer), AuthorizationEndpoint: testConsentPage})

	resp, err := http.PostForm(deps.server.URL+"/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"mcp_rt_x"}})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	var body map[string]string
	err = json.NewDecoder(resp.Body).Decode(&body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusServiceUnavailable || body["error"] != "temporarily_unavailable" ||
		resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("POST /oauth/token = %d %v %v (%v), want a no-store 503 temporarily_unavailable", resp.StatusCode, resp.Header, body, err)
	}

	resp = postMCP(t, deps.server.URL+"/mcp", "Authorization", "Bearer "+testAccessToken)
	if got := resp.Header.Get("WWW-Authenticate"); resp.StatusCode != http.StatusServiceUnavailable || got != "" {
		t.Errorf("POST /mcp = %d %q, want a 503 with no challenge", resp.StatusCode, got)
	}
}

// oauthBackend is a server with OAuth on over Postgres, a client whose metadata
// document is on a TLS server like the real ones, and a person signed in to the
// dashboard with an org and its default project.
type oauthBackend struct {
	t        *testing.T
	db       *testutil.TestPostgres
	url      string
	clientID string
	customer string
	project  string
	jwt      string
	profiles *stubProfiles
}

func newOAuthBackend(t *testing.T) *oauthBackend {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupPostgres(t)
	ctx := t.Context()
	b := &oauthBackend{t: t, db: db, customer: xid.New().String(), profiles: &stubProfiles{}}

	w := dbwrite.New(db.PgW)
	if _, err := w.CreateCustomer(ctx, dbwrite.CreateCustomerParams{ID: b.customer, Email: "ada@example.com"}); err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	orgs := coreorgs.NewService(db.PgRO, db.PgW, nil)
	org, err := orgs.CreateOrgWithDefaults(ctx, b.customer, "Example")
	if err != nil {
		t.Fatalf("CreateOrgWithDefaults: %v", err)
	}
	projects, err := dbread.New(db.PgW).GetProjectsByOrgID(ctx, org.ID)
	if err != nil || len(projects) != 1 {
		t.Fatalf("projects = %v, %v; want the default one", projects, err)
	}
	b.project = projects[0].ID
	session := xid.New().String()
	if _, err := w.CreateRefreshToken(ctx, dbwrite.CreateRefreshTokenParams{
		ID: xid.New().String(), CustomerID: b.customer, FamilyID: session, TokenHash: session,
		ExpiresAt: postgres.NewTimestamptz(time.Now().Add(time.Hour)),
	}); err != nil {
		t.Fatalf("CreateRefreshToken: %v", err)
	}
	b.jwt = signDashboardJWT(t, jwt.MapClaims{"sub": b.customer, "sid": session})

	// The document names itself by the URL it is served at.
	docs := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"client_id":     "https://" + r.Host + r.URL.Path,
			"client_name":   "Test client",
			"redirect_uris": []string{"http://localhost/callback"},
		})
	}))
	t.Cleanup(docs.Close)
	b.clientID = docs.URL + "/client.json"

	mux := http.NewServeMux()
	srv := httptest.NewServer(pogrpc.WithCorrelationID(mux))
	t.Cleanup(srv.Close)
	b.url = srv.URL
	grants := mcpgrants.NewService(db.PgW, cimd.NewClient(docs.Client()), srv.URL)

	authorizer, err := authz.NewAuthorizer()
	if err != nil {
		t.Fatalf("authz.NewAuthorizer: %v", err)
	}
	otelInterceptor, err := otelconnect.NewInterceptor()
	if err != nil {
		t.Fatalf("otelconnect.NewInterceptor: %v", err)
	}
	// A token acts as its person, so the role gate needs the real role lookup.
	opts := pogrpc.HandlerOptions(otelInterceptor, authorizer, orgs)
	queries := dbread.New(db.PgW)
	repo := &stubKeyLookup{}
	sharedMW := authn.NewMiddleware(pogrpc.WithDualAuth(testJWTKey, queries, repo, grants))
	pp, ph := profilesv1connect.NewProfilesServiceHandler(b.profiles, opts)
	mux.Handle(pp, sharedMW.Wrap(ph))
	sp, sh := sharedprojectsv1connect.NewProjectsServiceHandler(sharedprojectsrpc.NewServer(queries, grants), opts)
	mux.Handle(sp, sharedMW.Wrap(sh))
	cp, ch := mcpgrantsv1connect.NewMCPGrantsServiceHandler(mcpgrantsrpc.NewServer(grants), opts)
	mux.Handle(cp, authn.NewMiddleware(pogrpc.WithJWTAuth(testJWTKey, queries)).Wrap(ch))
	if err := Mount(mux, mux, repo, OAuth{Grants: grants, AuthorizationEndpoint: testConsentPage}); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	return b
}

// approve is the person's browser on the consent page: it approves the client's
// request, and pug sends it back with a code.
func (b *oauthBackend) approve(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
	u, err := url.Parse(args.URL)
	if err != nil {
		return nil, err
	}
	if page := u.Scheme + "://" + u.Host + u.Path; page != testConsentPage {
		return nil, fmt.Errorf("the client opened %s, not the consent page", page)
	}
	q := u.Query()
	req := connect.NewRequest(&mcpgrantsv1.ApproveAuthorizationRequest{
		Request: &mcpgrantsv1.AuthorizationRequest{
			ClientId:            proto.String(q.Get("client_id")),
			CodeChallenge:       proto.String(q.Get("code_challenge")),
			CodeChallengeMethod: proto.String(q.Get("code_challenge_method")),
			RedirectUri:         proto.String(q.Get("redirect_uri")),
			Resource:            proto.String(q.Get("resource")),
			ResponseType:        proto.String(q.Get("response_type")),
			State:               proto.String(q.Get("state")),
		},
	})
	req.Header().Set("Authorization", "Bearer "+b.jwt)
	resp, err := mcpgrantsv1connect.NewMCPGrantsServiceClient(http.DefaultClient, b.url).ApproveAuthorization(ctx, req)
	if err != nil {
		return nil, err
	}
	back, err := url.Parse(resp.Msg.GetRedirectUri())
	if err != nil {
		return nil, err
	}
	r := back.Query()
	return &auth.AuthorizationResult{Code: r.Get("code"), State: r.Get("state"), Iss: r.Get("iss")}, nil
}

// tokenRecorder notes each token request: whether it authenticated, and its status.
type tokenRecorder struct{ requests []string }

func (r *tokenRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err == nil && req.URL.Path == "/oauth/token" {
		kind := "form"
		if req.Header.Get("Authorization") != "" {
			kind = "basic"
		}
		r.requests = append(r.requests, fmt.Sprintf("%s %d", kind, resp.StatusCode))
	}
	return resp, err
}

// connect signs in the way a client does, through the go-sdk's own OAuth client.
// It returns the session, the tokens, and the token requests it took.
func (b *oauthBackend) connect() (*gomcp.ClientSession, *oauth2.Token, []string) {
	b.t.Helper()
	recorder := &tokenRecorder{}
	handler, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		ClientIDMetadataDocumentConfig: &auth.ClientIDMetadataDocumentConfig{URL: b.clientID},
		// Like Claude Code: a loopback redirect on a port of its choosing.
		RedirectURL:              "http://localhost:43117/callback",
		AuthorizationCodeFetcher: b.approve,
		Client:                   &http.Client{Transport: recorder},
	})
	if err != nil {
		b.t.Fatalf("NewAuthorizationCodeHandler: %v", err)
	}
	ctx, cancel := context.WithTimeout(b.t.Context(), 15*time.Second)
	b.t.Cleanup(cancel)
	session, err := gomcp.NewClient(&gomcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx,
		&gomcp.StreamableClientTransport{Endpoint: b.url + "/mcp", OAuthHandler: handler, DisableStandaloneSSE: true}, nil)
	if err != nil {
		b.t.Fatalf("Connect: %v", err)
	}
	b.t.Cleanup(func() { _ = session.Close() })
	ts, err := handler.TokenSource(ctx)
	if err != nil {
		b.t.Fatalf("TokenSource: %v", err)
	}
	tok, err := ts.Token()
	if err != nil {
		b.t.Fatalf("Token: %v", err)
	}
	return session, tok, recorder.requests
}

func TestE2E_OAuthSignIn(t *testing.T) {
	b := newOAuthBackend(t)
	session, tok, tokenRequests := b.connect()

	// Go's oauth2 tries Basic first, and the refusal must leave the code for the retry.
	if want := []string{"basic 401", "form 200"}; !slices.Equal(tokenRequests, want) {
		t.Errorf("token requests = %v, want %v", tokenRequests, want)
	}
	if !strings.HasPrefix(tok.AccessToken, mcpgrants.AccessTokenPrefix) || tok.RefreshToken == "" {
		t.Fatalf("token = %+v, want an MCP access token and a refresh token", tok)
	}

	res, err := session.CallTool(t.Context(), &gomcp.CallToolParams{
		Name: "get_profile", Arguments: map[string]any{"id": "user-1", "project_id": b.project},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool failed: %s", toolText(res))
	}
	if p := b.profiles.principal; p == nil || p.AuthType != pogrpc.AuthTypeMCPToken || p.Customer.ID != b.customer || p.Project.ID != b.project {
		t.Errorf("the tool call ran as %+v, want the person on their project", p)
	}

	// The token was issued for /mcp, so the Connect API refuses it.
	req, err := http.NewRequest(http.MethodPost, b.url+profilesv1connect.ProfilesServiceGetProcedure, strings.NewReader(`{"id":"user-1"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the Connect API answered an MCP token with %d, want 401", resp.StatusCode)
	}

	resp, err = http.PostForm(b.url+"/oauth/token", url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {tok.RefreshToken}, "client_id": {b.clientID},
	})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	var refreshed tokenResponse
	err = json.NewDecoder(resp.Body).Decode(&refreshed)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || refreshed.TokenType != "Bearer" || refreshed.ExpiresIn != 3600 ||
		refreshed.Scope != "read" || refreshed.AccessToken == tok.AccessToken || refreshed.RefreshToken == tok.RefreshToken {
		t.Fatalf("refresh = %d %+v, %v; want a new pair", resp.StatusCode, refreshed, err)
	}
	if resp := postMCP(t, b.url+"/mcp", "Authorization", "Bearer "+refreshed.AccessToken); resp.StatusCode != http.StatusOK {
		t.Errorf("POST /mcp with the refreshed token = %d, want 200", resp.StatusCode)
	}
}

// A connection ends when access does: the next call is a 401, and the refresh that
// follows is invalid_grant, which sends the client back to sign in.
func TestE2E_OAuthAccessEnds(t *testing.T) {
	for name, end := range map[string]func(*oauthBackend){
		"member removed": func(b *oauthBackend) { b.exec("delete from org_members where customer_id = $1", b.customer) },
	} {
		t.Run(name, func(t *testing.T) {
			b := newOAuthBackend(t)
			_, tok, _ := b.connect()
			bearer := "Bearer " + tok.AccessToken
			if resp := postMCP(t, b.url+"/mcp", "Authorization", bearer); resp.StatusCode != http.StatusOK {
				t.Fatalf("before: POST /mcp = %d, want 200", resp.StatusCode)
			}
			end(b)
			resp := postMCP(t, b.url+"/mcp", "Authorization", bearer)
			if got := resp.Header.Get("WWW-Authenticate"); resp.StatusCode != http.StatusUnauthorized ||
				!strings.HasPrefix(got, `Bearer error="invalid_token"`) {
				t.Errorf("after: POST /mcp = %d %q, want 401 with invalid_token", resp.StatusCode, got)
			}

			resp, err := http.PostForm(b.url+"/oauth/token", url.Values{
				"grant_type": {"refresh_token"}, "refresh_token": {tok.RefreshToken}, "client_id": {b.clientID},
			})
			if err != nil {
				t.Fatalf("refresh: %v", err)
			}
			var body map[string]string
			err = json.NewDecoder(resp.Body).Decode(&body)
			_ = resp.Body.Close()
			if err != nil || resp.StatusCode != http.StatusBadRequest || body["error"] != "invalid_grant" {
				t.Errorf("after: refresh = %d %v (%v), want 400 invalid_grant", resp.StatusCode, body, err)
			}
		})
	}
}

// joinOrg adds the person, as a viewer, to a new org someone else made, and
// returns that org's default project.
func (b *oauthBackend) joinOrg(name string) string {
	b.t.Helper()
	ctx := b.t.Context()
	w := dbwrite.New(b.db.PgW)
	owner := xid.New().String()
	if _, err := w.CreateCustomer(ctx, dbwrite.CreateCustomerParams{ID: owner, Email: owner + "@example.com"}); err != nil {
		b.t.Fatalf("CreateCustomer: %v", err)
	}
	org, err := coreorgs.NewService(b.db.PgRO, b.db.PgW, nil).CreateOrgWithDefaults(ctx, owner, name)
	if err != nil {
		b.t.Fatalf("CreateOrgWithDefaults: %v", err)
	}
	projects, err := dbread.New(b.db.PgW).GetProjectsByOrgID(ctx, org.ID)
	if err != nil || len(projects) != 1 {
		b.t.Fatalf("projects = %v, %v; want the default one", projects, err)
	}
	if _, err := w.CreateOrgMember(ctx, dbwrite.CreateOrgMemberParams{CustomerID: b.customer, OrgID: org.ID, Role: "ORG_ROLE_VIEWER"}); err != nil {
		b.t.Fatalf("CreateOrgMember: %v", err)
	}
	return projects[0].ID
}

// A connection reads every project in the orgs the person was in when they
// approved, one per call, and none in an org they joined later.
func TestE2E_OAuthReadsEveryOrg(t *testing.T) {
	b := newOAuthBackend(t)
	before := b.joinOrg("Globex")
	session, _, _ := b.connect()
	after := b.joinOrg("Initech")
	call := func(name string, args map[string]any) *gomcp.CallToolResult {
		t.Helper()
		res, err := session.CallTool(t.Context(), &gomcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("CallTool %s: %v", name, err)
		}
		return res
	}
	read := func(project string) *gomcp.CallToolResult {
		t.Helper()
		return call("get_profile", map[string]any{"id": "user-1", "project_id": project})
	}

	listed := toolText(call("list_projects", nil))
	if !strings.Contains(listed, b.project) || !strings.Contains(listed, before) || strings.Contains(listed, after) {
		t.Errorf("list_projects = %s, want the person's org and Globex, not Initech", listed)
	}
	for project, want := range map[string]bool{b.project: true, before: true, after: false} {
		if res := read(project); res.IsError == want {
			t.Errorf("get_profile on %s: error = %v (%s), want read = %v", project, res.IsError, toolText(res), want)
		} else if want && b.profiles.principal.Project.ID != project {
			t.Errorf("get_profile on %s ran on %s", project, b.profiles.principal.Project.ID)
		}
	}
	if res := call("get_profile", map[string]any{"id": "user-1"}); !res.IsError || !strings.Contains(toolText(res), "project_id is required") {
		t.Errorf("get_profile without project_id = %s, want a refusal that asks for it", toolText(res))
	}

	// A deleted project ends its own reads, not the connection.
	b.exec("update projects set deletion_time = now() where id = $1", before)
	if res := read(before); !res.IsError {
		t.Errorf("get_profile on a deleted project succeeded")
	}
	if res := read(b.project); res.IsError {
		t.Errorf("get_profile on a live project after another's deletion: %s", toolText(res))
	}
}

func (b *oauthBackend) exec(sql string, args ...any) {
	b.t.Helper()
	if _, err := b.db.PgW.Exec(b.t.Context(), sql, args...); err != nil {
		b.t.Fatalf("%s: %v", sql, err)
	}
}
