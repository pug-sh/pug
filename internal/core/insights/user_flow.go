package insights

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"

	chq "github.com/pug-sh/pug/internal/core/clickhouse"
	commonv1 "github.com/pug-sh/pug/internal/gen/proto/common/v1"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
	"google.golang.org/protobuf/proto"
)

const (
	defaultUserFlowMaxHops  = 5
	defaultUserFlowMaxNodes = 20 // per step (per depth)
	defaultUserFlowMaxLinks = 250

	// defaultUserFlowNodeKind is the resolved value when NodeKind is UNSPECIFIED.
	// Not a silent fallback — resolved explicitly in resolveUserFlowParams, not via
	// a switch fallthrough.
	defaultUserFlowNodeKind = insightsv1.UserFlowQuery_NODE_KIND_EVENT_KIND

	userFlowOthersNodeID = "$others"
)

type userFlowResolved struct {
	nodeKind     insightsv1.UserFlowQuery_NodeKind
	nodeProperty string
	maxHops      int32
	maxNodes     int32
	maxLinks     int32
}

func resolveUserFlowParams(uf *insightsv1.UserFlowQuery) userFlowResolved {
	r := userFlowResolved{
		nodeKind:     uf.GetNodeKind(),
		nodeProperty: uf.GetNodeProperty(),
		maxHops:      uf.GetMaxHops(),
		maxNodes:     uf.GetMaxNodes(),
		maxLinks:     uf.GetMaxLinks(),
	}
	if r.nodeKind == insightsv1.UserFlowQuery_NODE_KIND_UNSPECIFIED {
		r.nodeKind = defaultUserFlowNodeKind
	}
	if r.maxHops == 0 {
		r.maxHops = defaultUserFlowMaxHops
	}
	if r.maxNodes == 0 {
		r.maxNodes = defaultUserFlowMaxNodes
	}
	if r.maxLinks == 0 {
		r.maxLinks = defaultUserFlowMaxLinks
	}
	return r
}

func buildUserFlowQuery(req *insightsv1.QueryRequest, projectID string, resolved userFlowResolved) (*chq.Query, error) {
	groupKeyCol := userFlowSessionGroupKeyColumn()
	nodeExpr := userFlowNodeExpr(resolved.nodeKind, resolved.nodeProperty)

	topLevelFilterCond, err := buildTopLevelFilterCondition(
		req.GetSpec().GetFilterGroups(),
		req.GetSpec().GetFilterGroupsOperator(),
		projectID, "",
	)
	if err != nil {
		return nil, fmt.Errorf("user flow: %w", err)
	}
	scopeCond, err := userFlowScopeCondition(req.GetSpec().GetUserFlow(), projectID)
	if err != nil {
		return nil, fmt.Errorf("user flow: %w", err)
	}

	from := req.GetTimeRange().GetFrom().AsTime()
	to := req.GetTimeRange().GetTo().AsTime()
	maxHopsP1 := int(resolved.maxHops) + 1

	sessionNodesCTE := chq.NewQuery().
		Select(groupKeyCol+" AS group_key").
		SelectExpr(userFlowNodesArrayExpr(nodeExpr, maxHopsP1)+" AS nodes").
		From("events").
		Where(
			chq.Eq("project_id", projectID),
			chq.Gte("occur_time", from),
			chq.Lt("occur_time", to),
			scopeCond,
			topLevelFilterCond,
			userFlowNonEmptySessionKeyCond(),
		).
		GroupBy(groupKeyCol)

	// idx is 1-based (arrayEnumerate); toInt32(idx-1) is the 0-based depth of the
	// source node — the Sankey column. The edge connects depth (idx-1) → idx.
	pairsCTE := chq.NewQuery().
		Select(
			"group_key",
			"toInt32(idx - 1) AS step",
			"nodes[idx] AS source",
			"nodes[idx + 1] AS target",
		).
		From("session_nodes ARRAY JOIN arrayEnumerate(nodes) AS idx").
		Where(chq.RawCond("idx < length(nodes)"))

	// Group by step as well as source/target: the same label at two positions is
	// two distinct nodes, so transitions are position-scoped (Rybbit-style steps).
	// No "source != target" filter — a session that fires the same event twice in a
	// row is a legitimate depth d → d+1 step, never a self-loop (the endpoints sit
	// at different depths).
	return chq.NewQuery().
		With("session_nodes", sessionNodesCTE).
		With("pairs", pairsCTE).
		Select("step", "source", "target", "count(DISTINCT group_key) AS value").
		From("pairs").
		Where(
			chq.Neq("source", ""),
			chq.Neq("target", ""),
		).
		GroupBy("step", "source", "target").
		OrderBy("step ASC", "value DESC"), nil
}

// userFlowNodesArrayExpr returns the arraySlice/arraySort/groupArray expression.
// maxHopsP1 is max_hops+1 (edges → nodes). arraySlice offset is 1-indexed.
// Collect all (occur_time, node) tuples, sort by time, slice to first maxHopsP1.
// Do NOT use groupArray(N)(...) before sorting — size-limited groupArray retains
// arbitrary rows, not earliest by time.
func userFlowNodesArrayExpr(nodeExpr string, maxHopsP1 int) string {
	return fmt.Sprintf(
		"arraySlice(arrayMap(x -> x.2, arraySort(x -> x.1, groupArray((occur_time, %s)))), 1, %d)",
		nodeExpr, maxHopsP1,
	)
}

// userFlowSessionGroupKeyColumn returns the per-session grouping column. Only
// GROUP_BY_SESSION is implemented today; a future GROUP_BY_USER must add its own
// grouping column here (the proto enum already reserves the value).
func userFlowSessionGroupKeyColumn() string {
	return "session_id"
}

func userFlowNodeExpr(nodeKind insightsv1.UserFlowQuery_NodeKind, nodeProperty string) string {
	if nodeKind == insightsv1.UserFlowQuery_NODE_KIND_PROPERTY {
		return chq.PropertyExpr(nodeProperty)
	}
	return "kind"
}

// userFlowNonEmptySessionKeyCond excludes the nil UUID session_id.
func userFlowNonEmptySessionKeyCond() chq.Condition {
	return chq.Neq("session_id", "00000000-0000-0000-0000-000000000000")
}

func userFlowScopeCondition(uf *insightsv1.UserFlowQuery, projectID string) (chq.Condition, error) {
	if uf == nil || uf.GetScope() == nil {
		return chq.Condition{}, nil
	}
	return chq.EventConditionAliased([]*commonv1.EventFilter{uf.GetScope()}, projectID, "")
}

// depthLabel keys a real node by its (0-based depth, label). The same label at
// two depths is two distinct nodes — that layering is what makes the graph a
// clean DAG (every edge goes depth d → d+1), the property a Sankey needs.
type depthLabel struct {
	depth int32
	label string
}

// stepNodeRef is the internal identity of a flow-graph node: a label at a
// specific depth, plus whether it is that depth's synthetic overflow bucket.
// The bucket (others=true) is a DISTINCT identity from every real node at the
// same depth — including a real node literally named userFlowOthersNodeID — so
// collapsing pruned nodes never merges their traffic with a real "$others" node.
type stepNodeRef struct {
	depth  int32
	label  string
	others bool
}

// GroupUserFlowResult applies per-step top-N pruning, per-step $others collapse,
// and link truncation to raw step-scoped ClickHouse rows, returning a
// ready-to-serialize UserFlowResult.
//
// Each row is an edge from (Step, Source) at depth d to (Step+1, Target) at depth
// d+1. Ranking and the $others bucket are computed PER DEPTH, so each Sankey
// column keeps its own busiest nodes (a node ranked low globally but high at its
// step still survives in that column).
//
// The overflow bucket is tracked as a distinct identity (see stepNodeRef) and
// reported to clients via UserFlowNode.is_others — never by matching the id or
// label string. Each node also carries its depth so the client lays out columns
// directly instead of inferring them from the graph.
//
// No error return: this function is pure Go with no I/O. It always returns a
// non-nil result. On empty input it returns an empty but valid UserFlowResult.
func GroupUserFlowResult(_ context.Context, rows []UserFlowRow, maxNodes, maxLinks int) *insightsv1.UserFlowResult {
	empty := &insightsv1.UserFlowResult{}
	if len(rows) == 0 {
		return empty
	}

	// Weight each real (depth,label) node by total edge value touching it.
	nodeWeight := map[depthLabel]int64{}
	for _, row := range rows {
		nodeWeight[depthLabel{row.Step, row.Source}] += row.Value
		nodeWeight[depthLabel{row.Step + 1, row.Target}] += row.Value
	}

	// Per-step top-N: rank each depth's labels independently and keep the top
	// maxNodes; the rest collapse into that depth's $others bucket.
	labelsByDepth := map[int32][]depthLabel{}
	for dl := range nodeWeight {
		labelsByDepth[dl.depth] = append(labelsByDepth[dl.depth], dl)
	}
	kept := map[depthLabel]struct{}{}
	for _, dls := range labelsByDepth {
		sort.Slice(dls, func(i, j int) bool {
			if wi, wj := nodeWeight[dls[i]], nodeWeight[dls[j]]; wi != wj {
				return wi > wj
			}
			return dls[i].label < dls[j].label
		})
		limit := len(dls)
		if maxNodes > 0 && maxNodes < limit {
			limit = maxNodes
		}
		for _, dl := range dls[:limit] {
			kept[dl] = struct{}{}
		}
	}

	// remap routes a raw (depth,label) to a node identity: kept labels keep their
	// own identity (others=false); pruned labels collapse into their depth's
	// bucket, which stays distinct from a real node of the same label+depth.
	remap := func(depth int32, label string) stepNodeRef {
		if _, ok := kept[depthLabel{depth, label}]; ok {
			return stepNodeRef{depth: depth, label: label}
		}
		return stepNodeRef{depth: depth, label: userFlowOthersNodeID, others: true}
	}

	// Source sits at depth Step, target at depth Step+1 — always different depths,
	// so a remapped edge can never be a self-loop. Nothing to drop.
	aggregated := map[[2]stepNodeRef]int64{}
	for _, row := range rows {
		src := remap(row.Step, row.Source)
		tgt := remap(row.Step+1, row.Target)
		aggregated[[2]stepNodeRef{src, tgt}] += row.Value
	}

	type flowLink struct {
		source stepNodeRef
		target stepNodeRef
		value  int64
	}
	links := make([]flowLink, 0, len(aggregated))
	for key, value := range aggregated {
		if value <= 0 {
			continue
		}
		links = append(links, flowLink{source: key[0], target: key[1], value: value})
	}

	sort.Slice(links, func(i, j int) bool {
		if links[i].value != links[j].value {
			return links[i].value > links[j].value
		}
		if links[i].source.depth != links[j].source.depth {
			return links[i].source.depth < links[j].source.depth
		}
		if links[i].source.label != links[j].source.label {
			return links[i].source.label < links[j].source.label
		}
		if links[i].source.others != links[j].source.others {
			return !links[i].source.others // real before bucket
		}
		if links[i].target.label != links[j].target.label {
			return links[i].target.label < links[j].target.label
		}
		return !links[i].target.others
	})

	if maxLinks > 0 && len(links) > maxLinks {
		links = links[:maxLinks]
	}
	if len(links) == 0 {
		return empty
	}

	// Surviving node identities (after link truncation).
	nodeSet := map[stepNodeRef]struct{}{}
	for _, l := range links {
		nodeSet[l.source] = struct{}{}
		nodeSet[l.target] = struct{}{}
	}

	refs := slices.Collect(maps.Keys(nodeSet))
	// Assign a unique opaque id per identity. Base form "depth:label"; disambiguate
	// the rare clash (a real node literally named "$others" sharing a depth with
	// that depth's bucket) by appending "_". Assign real-before-bucket so the real
	// node keeps the clean id. is_others stays the authoritative bucket signal.
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].depth != refs[j].depth {
			return refs[i].depth < refs[j].depth
		}
		if refs[i].others != refs[j].others {
			return !refs[i].others
		}
		return refs[i].label < refs[j].label
	})
	idByRef := make(map[stepNodeRef]string, len(refs))
	usedIDs := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		id := fmt.Sprintf("%d:%s", ref.depth, ref.label)
		for {
			if _, clash := usedIDs[id]; !clash {
				break
			}
			id += "_"
		}
		usedIDs[id] = struct{}{}
		idByRef[ref] = id
	}

	protoLinks := make([]*insightsv1.UserFlowLink, 0, len(links))
	for _, l := range links {
		protoLinks = append(protoLinks, &insightsv1.UserFlowLink{
			Source: proto.String(idByRef[l.source]),
			Target: proto.String(idByRef[l.target]),
			Value:  proto.Int64(l.value),
		})
	}

	// Emit nodes in layout order: depth asc, then weight desc / label asc within a
	// depth, with that depth's bucket last.
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].depth != refs[j].depth {
			return refs[i].depth < refs[j].depth
		}
		if refs[i].others != refs[j].others {
			return !refs[i].others // bucket last within its depth
		}
		if wi, wj := nodeWeight[depthLabel{refs[i].depth, refs[i].label}], nodeWeight[depthLabel{refs[j].depth, refs[j].label}]; wi != wj {
			return wi > wj
		}
		return refs[i].label < refs[j].label
	})

	protoNodes := make([]*insightsv1.UserFlowNode, 0, len(refs))
	for _, ref := range refs {
		protoNodes = append(protoNodes, &insightsv1.UserFlowNode{
			Id:       proto.String(idByRef[ref]),
			IsOthers: proto.Bool(ref.others),
			Depth:    proto.Int32(ref.depth),
			Label:    proto.String(ref.label),
		})
	}

	return &insightsv1.UserFlowResult{Nodes: protoNodes, Links: protoLinks}
}
