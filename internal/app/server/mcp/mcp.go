// Package mcp mounts pug's read-only shared analytics API as a Model Context
// Protocol (MCP) endpoint at /mcp. It is a thin adapter: every MCP tool call is
// forwarded back through the server's own Connect stack in-process (see
// loopbackClient), so validation, authentication and authorization run exactly
// as they do for an external private-key API request. No business logic, auth or
// validation is duplicated here.
//
// Tools are generated from the shared protos by protoc-gen-go-mcp (the *v1mcp
// packages); this package selects the read-only subset, gives each a curated
// name, and serves them over the official modelcontextprotocol/go-sdk streamable
// HTTP transport in stateless mode.
package mcp

import (
	"net/http"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pug-sh/pug/internal/gen/proto/shared/activity/v1/activityv1connect"
	activityv1mcp "github.com/pug-sh/pug/internal/gen/proto/shared/activity/v1/activityv1mcp"
	"github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1/insightsv1connect"
	insightsv1mcp "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1/insightsv1mcp"
	"github.com/pug-sh/pug/internal/gen/proto/shared/profiles/v1/profilesv1connect"
	profilesv1mcp "github.com/pug-sh/pug/internal/gen/proto/shared/profiles/v1/profilesv1mcp"
	mcpruntime "github.com/redpanda-data/protoc-gen-go-mcp/pkg/runtime"
	"github.com/redpanda-data/protoc-gen-go-mcp/pkg/runtime/gosdk"
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
// initialization. It points them at the schema-discovery tools before querying.
const instructions = `pug is a product-analytics platform. These tools are read-only and scoped to a single project (the one your private API key belongs to).

Start by discovering what data exists:
- get_activity_filter_schema lists the event kinds and the property keys/types available to filter and break down by.
- get_activity_property_values (or get_insights_property_values) lists the observed values for a given property key.

Then answer questions:
- query_insights runs trends, funnel, retention, segmentation, user-flow and top-K analyses over events.
- explore_events and get_activity_feed inspect raw event streams; get_activity_heatmap shows activity by time.
- get_profile, get_profile_by_external_id and get_profile_stats look up individual user profiles and their activity.

Time windows and granularity are arguments on the query tools; call the schema tools first so you use valid event kinds and property keys.`

// NewHandler builds the /mcp streamable-HTTP handler. loopback is the server mux
// that each tool call is replayed through in-process.
//
// It returns an error if the generated tool set has drifted from the curated
// rename table (a tool with no curated name, or a stale table entry), so a
// codegen change that adds or renames a shared RPC fails server startup rather
// than silently shipping an unnamed or missing tool — the same fail-fast culture
// as the authz registry.
func NewHandler(loopback http.Handler) (http.Handler, error) {
	raw := gomcp.NewServer(
		&gomcp.Implementation{Name: serverName, Version: serverVersion},
		&gomcp.ServerOptions{Instructions: instructions},
	)
	srv := newRenamer(gosdk.Wrap(raw))
	registerTools(srv, loopback)
	if err := srv.checkComplete(); err != nil {
		return nil, err
	}

	return gomcp.NewStreamableHTTPHandler(
		func(*http.Request) *gomcp.Server { return raw },
		&gomcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	), nil
}

// registerTools forwards each read-only shared RPC to the MCP server through an
// in-process loopback Connect client. srv is the rename-wrapping MCPServer; the
// profiles service filters out the GDPR erasure RPCs. Shared by NewHandler and
// the registration unit test so both exercise the identical wiring.
func registerTools(srv mcpruntime.MCPServer, loopback http.Handler) {
	client := &loopbackClient{handler: loopback}
	insightsv1mcp.ForwardToConnectInsightsServiceClient(srv,
		insightsv1connect.NewInsightsServiceClient(client, loopbackBaseURL),
		mcpruntime.WithToolFilter(keepInsightsTool))
	activityv1mcp.ForwardToConnectActivityServiceClient(srv,
		activityv1connect.NewActivityServiceClient(client, loopbackBaseURL))
	profilesv1mcp.ForwardToConnectProfilesServiceClient(srv,
		profilesv1connect.NewProfilesServiceClient(client, loopbackBaseURL),
		mcpruntime.WithToolFilter(keepProfileTool))
}
