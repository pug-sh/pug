// Package mcp mounts pug's shared analytics API as a Model Context Protocol
// (MCP) endpoint at /mcp. It is a thin adapter: every MCP tool call is
// forwarded back through the server's own Connect stack in-process (see
// loopbackClient), so validation, authentication and authorization run as for an
// external API request, except that only the loopback admits an MCP access token.
// No business logic, auth or validation is duplicated here. With OAuth on, it also
// serves the token endpoint and the metadata clients discover it from (oauth.go).
//
// Tools are generated from the shared protos by protoc-gen-go-mcp (the *v1mcp
// packages); this package selects the ones to expose, gives each a curated
// name and title, and serves them over the official modelcontextprotocol/go-sdk
// streamable HTTP transport in stateless mode.
package mcp

import (
	"net/http"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pug-sh/pug/internal/gen/proto/shared/activity/v1/activityv1connect"
	activityv1mcp "github.com/pug-sh/pug/internal/gen/proto/shared/activity/v1/activityv1mcp"
	"github.com/pug-sh/pug/internal/gen/proto/shared/dashboards/v1/shareddashboardsv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/shared/dashboards/v1/shareddashboardsv1mcp"
	"github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1/insightsv1connect"
	insightsv1mcp "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1/insightsv1mcp"
	"github.com/pug-sh/pug/internal/gen/proto/shared/profiles/v1/profilesv1connect"
	profilesv1mcp "github.com/pug-sh/pug/internal/gen/proto/shared/profiles/v1/profilesv1mcp"
	"github.com/pug-sh/pug/internal/gen/proto/shared/projects/v1/sharedprojectsv1connect"
	"github.com/pug-sh/pug/internal/gen/proto/shared/projects/v1/sharedprojectsv1mcp"
	mcpruntime "github.com/redpanda-data/protoc-gen-go-mcp/pkg/runtime"
)

const (
	serverName    = "pug"
	serverVersion = "0.1.0"

	// loopbackBaseURL is a placeholder base URL for the in-process Connect
	// clients. The loopback routes by URL path through the server mux and ignores
	// the host, so the value only has to be a syntactically valid absolute URL.
	loopbackBaseURL = "http://mcp.loopback"
)

// instructions is the MCP server's guidance to agents, returned during
// initialization. It points them at the schema-discovery tools before querying,
// and — critically — separates the project-wide tools from the per-profile ones,
// because reaching for a per-profile tool to answer a project-wide question is the
// most likely way for an agent to go wrong here.
//
// Every exposed tool must be named here and no hidden one may be:
// TestInstructionsMatchToolPolicy pins this against toolPolicy, since prose has no
// compile-time link to the tool table.
const instructions = `pug is a product-analytics platform. Every tool reads, except upsert_dashboard, which creates and edits dashboards.

Every tool but list_projects works in one project, named by its project_id argument:
- list_projects lists the projects you can read, each with its org. Call it first, and pass the id of the project the question is about as project_id to every other tool. When more than one project could fit and the question doesn't say which, ask.

Start by discovering what data exists:
- get_insights_filter_schema (or get_activity_filter_schema) lists the event kinds and the property keys/types available to filter and break down by.
- get_insights_property_values (or get_activity_property_values) lists the observed values for a given property key.

Project-wide questions ("how many", "what is trending", "where do users drop off"):
- query_insights is the main analysis tool: trends, funnel, retention, segmentation, user flow (Sankey), top-K and map (a metric per country) over the project's events. Reach for this for anything aggregate or over-time.
- explore_events browses the raw event stream across all users.

Individual users:
- get_profile (by pug profile id) and get_profile_by_external_id (by the id your application assigned) look up one user.
- get_activity_feed, get_activity_heatmap, get_profile_sessions and get_profile_stats each describe a SINGLE user and require that user's distinct_id, which a profile lookup gives you. They cannot answer project-wide questions — use query_insights for those.

Dashboards:
- list_dashboards lists the project's dashboards. get_dashboard returns one with its tiles and its update_time.
- upsert_dashboard creates a dashboard, or edits one given its id and the update_time from get_dashboard or your last upsert_dashboard. Get each insight tile right with query_insights first. Tiles you leave out stay as they are. Remove a tile only when the person asks.

Compliance:
- get_deletion_request reports the status of a previously submitted data-erasure request.

Time windows and granularity are arguments on the query tools; call the schema tools first so you use valid event kinds and property keys.`

// newHandler builds the /mcp streamable-HTTP handler. loopback is the server mux
// that each tool call is replayed through in-process. It is unexported so Mount
// stays the only way to obtain a /mcp handler — the handler is useless (and
// unauthenticated) without the middleware sandwich Mount wraps around it.
//
// It returns an error if the generated tool set has drifted from the curated
// policy table (a tool with no disposition, or a stale entry), so a codegen change
// that adds or renames a shared RPC fails server startup rather than silently
// shipping an unnamed or missing tool.
//
// Stateless is load-bearing, not a tuning knob: it makes the go-sdk build a fresh
// session per HTTP request and thread that request's context into the tool
// handler, which is how loopbackClient resolves the caller's own credential. In
// stateful mode the session context is captured once at initialize and reused for
// every later call on that session, which would turn per-request credential
// scoping into a stale-credential bug.
func newHandler(loopback http.Handler) (http.Handler, error) {
	raw := gomcp.NewServer(
		&gomcp.Implementation{Name: serverName, Version: serverVersion},
		&gomcp.ServerOptions{Instructions: instructions},
	)
	if err := registerRenamed(raw, func(srv mcpruntime.MCPServer) {
		registerTools(srv, loopback)
	}); err != nil {
		return nil, err
	}

	return gomcp.NewStreamableHTTPHandler(
		func(*http.Request) *gomcp.Server { return raw },
		&gomcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	), nil
}

// projectArg is the argument every tool but list_projects takes. The loopback sends
// it on as x-project-id, where auth checks the credential reads that project.
var projectArg = mcpruntime.ExtraProperty{
	Name:        "project_id",
	Description: "The id of the project, from list_projects.",
	ContextKey:  projectIDKey{},
}

// registerTools forwards each exposed shared RPC to the MCP server through an
// in-process loopback Connect client. srv is the rename-wrapping MCPServer, and
// the one keepTool filter withholds every tool the policy table hides (the GDPR
// erasure RPCs and the WIP insights SegmentUsers). Shared by newHandler and the
// registration unit test so both exercise the identical wiring.
func registerTools(srv mcpruntime.MCPServer, loopback http.Handler) {
	client := &loopbackClient{handler: loopback}
	filter := mcpruntime.WithToolFilter(keepTool)
	project := mcpruntime.WithExtraProperties(projectArg)

	sharedprojectsv1mcp.ForwardToConnectProjectsServiceClient(srv,
		sharedprojectsv1connect.NewProjectsServiceClient(client, loopbackBaseURL), filter)
	shareddashboardsv1mcp.ForwardToConnectDashboardsServiceClient(srv,
		shareddashboardsv1connect.NewDashboardsServiceClient(client, loopbackBaseURL), filter, project)
	insightsv1mcp.ForwardToConnectInsightsServiceClient(srv,
		insightsv1connect.NewInsightsServiceClient(client, loopbackBaseURL), filter, project)
	activityv1mcp.ForwardToConnectActivityServiceClient(srv,
		activityv1connect.NewActivityServiceClient(client, loopbackBaseURL), filter, project)
	profilesv1mcp.ForwardToConnectProfilesServiceClient(srv,
		profilesv1connect.NewProfilesServiceClient(client, loopbackBaseURL), filter, project)
}
