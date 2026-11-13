package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
	activityv1mcp "github.com/pug-sh/pug/internal/gen/proto/shared/activity/v1/activityv1mcp"
	"github.com/pug-sh/pug/internal/gen/proto/shared/dashboards/v1/shareddashboardsv1mcp"
	insightsv1mcp "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1/insightsv1mcp"
	profilesv1mcp "github.com/pug-sh/pug/internal/gen/proto/shared/profiles/v1/profilesv1mcp"
	"github.com/pug-sh/pug/internal/gen/proto/shared/projects/v1/sharedprojectsv1mcp"
	mcpruntime "github.com/redpanda-data/protoc-gen-go-mcp/pkg/runtime"
)

// toolDisposition is what happens to one generated tool: it is either exposed
// under a curated MCP name and title, or hidden with a recorded reason. The fields
// are unexported and the only constructors are expose, exposeWrite and hide, so
// the invariants "exposed XOR hidden" and "exposed implies a non-empty curated
// name" hold BY CONSTRUCTION rather than by test, the same idiom as
// authzspec.Spec.
type toolDisposition struct {
	curated string // non-empty iff exposed
	title   string // what clients show people in place of the curated name
	writes  bool   // the tool changes data
	reason  string // non-empty iff hidden
}

func expose(curated, title string) toolDisposition {
	return toolDisposition{curated: curated, title: title}
}

// exposeWrite exposes a tool that changes data. It is marked destructive, not
// read-only, so Claude asks the person before each call.
func exposeWrite(curated, title string) toolDisposition {
	return toolDisposition{curated: curated, title: title, writes: true}
}
func hide(reason string) toolDisposition { return toolDisposition{reason: reason} }

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
//
// expose marks a tool read-only, so Claude runs it without asking: a tool that
// writes uses exposeWrite.
var toolPolicy = map[string]toolDisposition{
	// projects
	sharedprojectsv1mcp.ProjectsService_ListTool.Name: expose("list_projects", "List projects"),

	// dashboards
	shareddashboardsv1mcp.DashboardsService_ListTool.Name:   expose("list_dashboards", "List dashboards"),
	shareddashboardsv1mcp.DashboardsService_GetTool.Name:    expose("get_dashboard", "Get dashboard"),
	shareddashboardsv1mcp.DashboardsService_UpsertTool.Name: exposeWrite("upsert_dashboard", "Create or update dashboard"),

	// insights
	insightsv1mcp.InsightsService_QueryTool.Name:             expose("query_insights", "Query insights"),
	insightsv1mcp.InsightsService_GetFilterSchemaTool.Name:   expose("get_insights_filter_schema", "Get insights filter schema"),
	insightsv1mcp.InsightsService_GetPropertyValuesTool.Name: expose("get_insights_property_values", "Get insights property values"),
	insightsv1mcp.InsightsService_SegmentUsersTool.Name: hide(
		"the segmentation drill-down insight is still WIP; served as a Connect RPC but kept off the tool surface until it is live"),

	// activity
	activityv1mcp.ActivityService_GetActivityFeedTool.Name:    expose("get_activity_feed", "Get activity feed"),
	activityv1mcp.ActivityService_GetEventExplorerTool.Name:   expose("explore_events", "Explore events"),
	activityv1mcp.ActivityService_GetFilterSchemaTool.Name:    expose("get_activity_filter_schema", "Get activity filter schema"),
	activityv1mcp.ActivityService_GetPropertyValuesTool.Name:  expose("get_activity_property_values", "Get activity property values"),
	activityv1mcp.ActivityService_GetActivityHeatmapTool.Name: expose("get_activity_heatmap", "Get activity heatmap"),
	activityv1mcp.ActivityService_GetProfileStatsTool.Name:    expose("get_profile_stats", "Get profile stats"),
	activityv1mcp.ActivityService_GetProfileSessionsTool.Name: expose("get_profile_sessions", "Get profile sessions"),

	// profiles
	profilesv1mcp.ProfilesService_GetTool.Name:                expose("get_profile", "Get profile"),
	profilesv1mcp.ProfilesService_GetByExternalIdTool.Name:    expose("get_profile_by_external_id", "Get profile by external ID"),
	profilesv1mcp.ProfilesService_GetDeletionRequestTool.Name: expose("get_deletion_request", "Get deletion request"),
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

type toolServer interface {
	AddTool(*gomcp.Tool, gomcp.ToolHandler)
}

// renamer registers each generated tool on the go-sdk server under its curated
// name, title and annotations, rejecting anything the policy does not sanction.
// It implements mcpruntime.MCPServer so the generated ForwardToConnect*
// functions register through it transparently.
//
// It is not safe for concurrent use and is not meant to outlive one registration
// pass; registerRenamed owns its whole lifetime, so neither is reachable.
type renamer struct {
	inner   toolServer
	seen    map[string]bool   // generated name -> registered
	claimed map[string]string // curated name -> the generated name that claimed it
	errs    []error
}

// registerRenamed runs register against a rename-wrapping MCPServer and returns
// the accumulated drift error. The *renamer never escapes this function, so a
// caller CANNOT register tools and then forget the completeness check — which
// would silently drop every tool the policy does not name (AddTool returns without
// registering) and boot a green server with a hole in its tool surface.
func registerRenamed(inner toolServer, register func(mcpruntime.MCPServer)) error {
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
	r.inner.AddTool(sdkTool(d, tool), sdkHandler(handler))
}

// sdkTool and sdkHandler stand in for gosdk.Wrap, which can't set a title or annotations.
func sdkTool(d toolDisposition, tool mcpruntime.Tool) *gomcp.Tool {
	t := &gomcp.Tool{
		Name:        d.curated,
		Title:       d.title,
		Description: tool.Description,
		InputSchema: tool.RawInputSchema,
		// Clients on MCP specs before 2025-06-18 read the title only from the annotations.
		// ChatGPT's directory wants every hint explicit, and no tool reaches past pug's own data.
		Annotations: &gomcp.ToolAnnotations{
			Title:           d.title,
			ReadOnlyHint:    !d.writes,
			DestructiveHint: new(d.writes),
			OpenWorldHint:   new(false),
		},
	}
	if len(tool.RawOutputSchema) > 0 {
		t.OutputSchema = tool.RawOutputSchema
	}

	return t
}

func sdkHandler(handler mcpruntime.ToolHandler) gomcp.ToolHandler {
	return func(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
		var args map[string]any
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return nil, err
			}
		}
		if args == nil {
			args = map[string]any{}
		}

		res, err := handler(ctx, &mcpruntime.CallToolRequest{Arguments: args})
		if err != nil || res == nil {
			return nil, err
		}

		return &gomcp.CallToolResult{
			Content:           []gomcp.Content{&gomcp.TextContent{Text: res.Text}},
			StructuredContent: res.StructuredContent,
			IsError:           res.IsError,
		}, nil
	}
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
