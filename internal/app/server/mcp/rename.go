package mcp

import (
	"errors"
	"fmt"
	"slices"

	activityv1mcp "github.com/pug-sh/pug/internal/gen/proto/shared/activity/v1/activityv1mcp"
	insightsv1mcp "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1/insightsv1mcp"
	profilesv1mcp "github.com/pug-sh/pug/internal/gen/proto/shared/profiles/v1/profilesv1mcp"
	mcpruntime "github.com/redpanda-data/protoc-gen-go-mcp/pkg/runtime"
)

// toolDisposition is what happens to one generated tool: it is either exposed
// under a curated MCP name, or hidden with a recorded reason. The fields are
// unexported and the only constructors are expose and hide, so the invariants
// "exposed XOR hidden" and "exposed implies a non-empty curated name" hold BY
// CONSTRUCTION rather than by test — the same idiom as authzspec.Spec.
type toolDisposition struct {
	curated string // non-empty iff exposed
	reason  string // non-empty iff hidden
}

func expose(curated string) toolDisposition { return toolDisposition{curated: curated} }
func hide(reason string) toolDisposition    { return toolDisposition{reason: reason} }

func (d toolDisposition) exposed() bool { return d.curated != "" }

// toolPolicy is the single source of truth for the MCP tool surface: one row per
// generated tool, either naming it or explaining why it is withheld. Naming and
// exclusion used to live in two separate structures (a rename table plus per-
// service filter predicates) that had to agree; here they are the same cell, so
// they cannot disagree.
//
// It is keyed off the generated Tool vars, so an RPC that is renamed or removed
// upstream breaks this table at COMPILE time. A generated tool with no row here,
// a row no generated tool matched, and two rows claiming the same curated name are
// all rejected at server startup by renamer/checkComplete.
//
// Hidden tools are filtered out before registration (see keepTool) AND rejected by
// the renamer if a filter regression ever let one through — so the GDPR erasure
// RPCs cannot reach an LLM agent even if the filter breaks.
var toolPolicy = map[string]toolDisposition{
	// insights
	insightsv1mcp.InsightsService_QueryTool.Name:             expose("query_insights"),
	insightsv1mcp.InsightsService_GetFilterSchemaTool.Name:   expose("get_insights_filter_schema"),
	insightsv1mcp.InsightsService_GetPropertyValuesTool.Name: expose("get_insights_property_values"),
	insightsv1mcp.InsightsService_SegmentUsersTool.Name: hide(
		"the segmentation drill-down insight is still WIP; served as a Connect RPC but kept off the tool surface until it is live"),

	// activity
	activityv1mcp.ActivityService_GetActivityFeedTool.Name:    expose("get_activity_feed"),
	activityv1mcp.ActivityService_GetEventExplorerTool.Name:   expose("explore_events"),
	activityv1mcp.ActivityService_GetFilterSchemaTool.Name:    expose("get_activity_filter_schema"),
	activityv1mcp.ActivityService_GetPropertyValuesTool.Name:  expose("get_activity_property_values"),
	activityv1mcp.ActivityService_GetActivityHeatmapTool.Name: expose("get_activity_heatmap"),
	activityv1mcp.ActivityService_GetProfileStatsTool.Name:    expose("get_profile_stats"),

	// profiles
	profilesv1mcp.ProfilesService_GetTool.Name:                expose("get_profile"),
	profilesv1mcp.ProfilesService_GetByExternalIdTool.Name:    expose("get_profile_by_external_id"),
	profilesv1mcp.ProfilesService_GetDeletionRequestTool.Name: expose("get_deletion_request"),
	profilesv1mcp.ProfilesService_DeleteTool.Name: hide(
		"GDPR/DPDP erasure is irreversible and must never be an LLM-callable tool"),
	profilesv1mcp.ProfilesService_DeleteDataSubjectTool.Name: hide(
		"GDPR/DPDP erasure is irreversible and must never be an LLM-callable tool"),
}

// keepTool is the single WithToolFilter predicate for every service, replacing the
// per-service predicates that could drift from the naming table.
//
// A tool the policy does not know about deliberately PASSES, so the renamer
// rejects it and startup fails: an unrecognised tool means "decide what to do with
// this", never "silently drop it".
func keepTool(generatedName string) bool {
	d, ok := toolPolicy[generatedName]
	return !ok || d.exposed()
}

// renamer wraps a mcpruntime.MCPServer and rewrites each generated tool name to
// its curated name at registration time, rejecting anything the policy does not
// sanction. It implements mcpruntime.MCPServer so the generated ForwardToConnect*
// functions register through it transparently.
//
// It is not safe for concurrent use and is not meant to outlive one registration
// pass; registerRenamed owns its whole lifetime, so neither is reachable.
type renamer struct {
	inner   mcpruntime.MCPServer
	seen    map[string]bool   // generated name -> registered
	claimed map[string]string // curated name -> the generated name that claimed it
	errs    []error
}

// registerRenamed runs register against a rename-wrapping MCPServer and returns
// the accumulated drift error. The *renamer never escapes this function, so a
// caller CANNOT register tools and then forget the completeness check — which
// would silently drop every tool the policy does not name (AddTool returns without
// registering) and boot a green server with a hole in its tool surface.
func registerRenamed(inner mcpruntime.MCPServer, register func(mcpruntime.MCPServer)) error {
	r := &renamer{
		inner:   inner,
		seen:    make(map[string]bool),
		claimed: make(map[string]string),
	}
	register(r)

	return r.checkComplete()
}

func (r *renamer) AddTool(tool mcpruntime.Tool, handler mcpruntime.ToolHandler) {
	d, ok := toolPolicy[tool.Name]
	if !ok {
		r.errs = append(r.errs, fmt.Errorf("mcp: generated tool %q has no entry in the tool policy table", tool.Name))
		return
	}
	if !d.exposed() {
		// Unreachable while keepTool is the filter, which is the point: if a filter
		// regression ever let a hidden tool through, it fails startup instead of
		// silently reaching an LLM agent.
		r.errs = append(r.errs, fmt.Errorf("mcp: hidden tool %q reached registration (%s)", tool.Name, d.reason))
		return
	}
	if prev, dup := r.claimed[d.curated]; dup {
		// The go-sdk's feature set REPLACES a tool registered under an existing name,
		// so a duplicate curated name would silently shadow one of the two tools.
		r.errs = append(r.errs, fmt.Errorf(
			"mcp: curated name %q is claimed by both %q and %q", d.curated, prev, tool.Name))
		return
	}

	r.claimed[d.curated] = tool.Name
	r.seen[tool.Name] = true
	tool.Name = d.curated
	r.inner.AddTool(tool, handler)
}

// checkComplete reports any exposed policy entry that no generated tool matched (a
// stale row, left behind after an RPC is removed upstream), plus every error
// AddTool accumulated. Hidden entries are never registered by design, so they are
// exempt from the staleness check — a hidden row that goes stale is caught at
// compile time instead, since the keys are the generated Tool vars.
func (r *renamer) checkComplete() error {
	if len(r.errs) > 0 {
		return errors.Join(r.errs...)
	}

	var stale []string
	for generated, d := range toolPolicy {
		if d.exposed() && !r.seen[generated] {
			stale = append(stale, generated)
		}
	}
	if len(stale) > 0 {
		slices.Sort(stale)
		return fmt.Errorf("mcp: tool policy has %d stale entr(y/ies) with no matching generated tool: %v", len(stale), stale)
	}

	return nil
}
