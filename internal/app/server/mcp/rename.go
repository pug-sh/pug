package mcp

import (
	"errors"
	"fmt"
	"sort"

	activityv1mcp "github.com/pug-sh/pug/internal/gen/proto/shared/activity/v1/activityv1mcp"
	insightsv1mcp "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1/insightsv1mcp"
	profilesv1mcp "github.com/pug-sh/pug/internal/gen/proto/shared/profiles/v1/profilesv1mcp"
	mcpruntime "github.com/redpanda-data/protoc-gen-go-mcp/pkg/runtime"
)

// toolRenames maps each generated tool's verbose name
// (e.g. "shared_insights_v1_InsightsService_Query") to the curated MCP tool name
// exposed to clients (e.g. "query_insights"). It is keyed off the generated Tool
// vars, so an RPC that is renamed or removed upstream breaks this table at
// COMPILE time — the strongest possible drift guard. A generated tool missing
// from the table, or a stale table entry, additionally fails server startup via
// renamer.checkComplete.
//
// The excluded tools — profiles Delete / DeleteDataSubject (GDPR/DPDP erasure)
// and insights SegmentUsers (served as a Connect RPC but a WIP, kept off the MCP
// surface) — are filtered out before registration (see keepProfileTool /
// keepInsightsTool) and are deliberately absent here, so if a filter ever
// regressed the renamer would reject the tool rather than silently expose it.
var toolRenames = map[string]string{
	insightsv1mcp.InsightsService_QueryTool.Name:             "query_insights",
	insightsv1mcp.InsightsService_GetFilterSchemaTool.Name:   "get_insights_filter_schema",
	insightsv1mcp.InsightsService_GetPropertyValuesTool.Name: "get_insights_property_values",

	activityv1mcp.ActivityService_GetActivityFeedTool.Name:    "get_activity_feed",
	activityv1mcp.ActivityService_GetEventExplorerTool.Name:   "explore_events",
	activityv1mcp.ActivityService_GetFilterSchemaTool.Name:    "get_activity_filter_schema",
	activityv1mcp.ActivityService_GetPropertyValuesTool.Name:  "get_activity_property_values",
	activityv1mcp.ActivityService_GetActivityHeatmapTool.Name: "get_activity_heatmap",
	activityv1mcp.ActivityService_GetProfileStatsTool.Name:    "get_profile_stats",

	profilesv1mcp.ProfilesService_GetTool.Name:                "get_profile",
	profilesv1mcp.ProfilesService_GetByExternalIdTool.Name:    "get_profile_by_external_id",
	profilesv1mcp.ProfilesService_GetDeletionRequestTool.Name: "get_deletion_request",
}

// keepProfileTool is the WithToolFilter predicate for the profiles service: it
// excludes the irreversible GDPR/DPDP erasure RPCs (Delete, DeleteDataSubject)
// so they are never registered as MCP tools. Every other generated profiles tool
// passes through to the renamer.
func keepProfileTool(generatedName string) bool {
	switch generatedName {
	case profilesv1mcp.ProfilesService_DeleteTool.Name,
		profilesv1mcp.ProfilesService_DeleteDataSubjectTool.Name:
		return false
	}
	return true
}

// keepInsightsTool is the WithToolFilter predicate for the insights service: it
// excludes SegmentUsers, which is served as a Connect RPC but deliberately kept
// off the MCP tool surface while the insight is a work in progress (not yet
// live). Every other generated insights tool passes through to the renamer.
func keepInsightsTool(generatedName string) bool {
	return generatedName != insightsv1mcp.InsightsService_SegmentUsersTool.Name
}

// renamer wraps a mcpruntime.MCPServer and rewrites each tool's generated name
// to its curated name at registration time. It implements mcpruntime.MCPServer
// so the generated ForwardToConnect* functions register through it transparently.
//
// It fails fast: AddTool records an error if a generated tool has no curated
// name, and checkComplete additionally reports any table entry that no generated
// tool matched (a stale rename). Both are surfaced at server startup.
type renamer struct {
	inner mcpruntime.MCPServer
	seen  map[string]bool
	errs  []error
}

func newRenamer(inner mcpruntime.MCPServer) *renamer {
	return &renamer{inner: inner, seen: make(map[string]bool)}
}

func (r *renamer) AddTool(tool mcpruntime.Tool, handler mcpruntime.ToolHandler) {
	curated, ok := toolRenames[tool.Name]
	if !ok {
		r.errs = append(r.errs, fmt.Errorf("mcp: generated tool %q has no curated name in the rename table", tool.Name))
		return
	}
	r.seen[tool.Name] = true
	tool.Name = curated
	r.inner.AddTool(tool, handler)
}

// checkComplete returns a non-nil error if any generated tool was missing from
// the rename table, or if any table entry was never consumed by a generated tool
// (a stale entry). Call it once, after all services have been registered.
func (r *renamer) checkComplete() error {
	if len(r.errs) > 0 {
		return errors.Join(r.errs...)
	}
	var stale []string
	for generated := range toolRenames {
		if !r.seen[generated] {
			stale = append(stale, generated)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		return fmt.Errorf("mcp: rename table has %d stale entr(y/ies) with no matching generated tool: %v", len(stale), stale)
	}
	return nil
}
