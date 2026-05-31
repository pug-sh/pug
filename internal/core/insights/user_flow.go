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
	defaultUserFlowMaxNodes = 20
	defaultUserFlowMaxLinks = 100

	// defaultUserFlowGroupBy is the resolved value when GroupBy is UNSPECIFIED.
	// Not a silent fallback — used explicitly at the top of BuildUserFlowQuery.
	defaultUserFlowGroupBy = insightsv1.UserFlowQuery_GROUP_BY_SESSION

	// defaultUserFlowNodeKind is the resolved value when NodeKind is UNSPECIFIED.
	// Used explicitly at the top of BuildUserFlowQuery — not a switch fallthrough.
	defaultUserFlowNodeKind = insightsv1.UserFlowQuery_NODE_KIND_EVENT_KIND

	userFlowOthersNodeID = "$others"
)

type userFlowResolved struct {
	groupBy      insightsv1.UserFlowQuery_GroupBy
	nodeKind     insightsv1.UserFlowQuery_NodeKind
	nodeProperty string
	maxHops      int32
	maxNodes     int32
	maxLinks     int32
}

func resolveUserFlowParams(uf *insightsv1.UserFlowQuery) userFlowResolved {
	r := userFlowResolved{
		groupBy:      uf.GetGroupBy(),
		nodeKind:     uf.GetNodeKind(),
		nodeProperty: uf.GetNodeProperty(),
		maxHops:      uf.GetMaxHops(),
		maxNodes:     uf.GetMaxNodes(),
		maxLinks:     uf.GetMaxLinks(),
	}
	if r.groupBy == insightsv1.UserFlowQuery_GROUP_BY_UNSPECIFIED {
		r.groupBy = defaultUserFlowGroupBy
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
	groupKeyCol := userFlowGroupKeyColumn(resolved.groupBy)
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
		Select(groupKeyCol + " AS group_key").
		SelectExpr(userFlowNodesArrayExpr(nodeExpr, maxHopsP1) + " AS nodes").
		From("events").
		Where(
			chq.Eq("project_id", projectID),
			chq.Gte("occur_time", from),
			chq.Lt("occur_time", to),
			scopeCond,
			topLevelFilterCond,
			userFlowNonEmptyGroupKeyCond(resolved.groupBy),
		).
		GroupBy(groupKeyCol)

	pairsCTE := chq.NewQuery().
		Select(
			"group_key",
			"nodes[idx] AS source",
			"nodes[idx + 1] AS target",
		).
		From("session_nodes ARRAY JOIN arrayEnumerate(nodes) AS idx").
		Where(chq.RawCond("idx < length(nodes)"))

	return chq.NewQuery().
		With("session_nodes", sessionNodesCTE).
		With("pairs", pairsCTE).
		Select("source", "target", "count(DISTINCT group_key) AS value").
		From("pairs").
		Where(
			chq.Neq("source", ""),
			chq.Neq("target", ""),
			chq.RawCond("source != target"),
		).
		GroupBy("source", "target").
		OrderBy("value DESC"), nil
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

func userFlowGroupKeyColumn(_ insightsv1.UserFlowQuery_GroupBy) string {
	return "session_id"
}

func userFlowNodeExpr(nodeKind insightsv1.UserFlowQuery_NodeKind, nodeProperty string) string {
	if nodeKind == insightsv1.UserFlowQuery_NODE_KIND_PROPERTY {
		return chq.PropertyExpr(nodeProperty)
	}
	return "kind"
}

// userFlowNonEmptyGroupKeyCond excludes the nil UUID session_id.
func userFlowNonEmptyGroupKeyCond(_ insightsv1.UserFlowQuery_GroupBy) chq.Condition {
	return chq.Neq("session_id", "00000000-0000-0000-0000-000000000000")
}

func userFlowScopeCondition(uf *insightsv1.UserFlowQuery, projectID string) (chq.Condition, error) {
	if uf == nil || uf.GetScope() == nil {
		return chq.Condition{}, nil
	}
	return chq.EventConditionAliased([]*commonv1.EventFilter{uf.GetScope()}, projectID, "")
}

// GroupUserFlowResult applies top-N pruning, $others collapse, and link truncation
// to raw ClickHouse rows and returns a ready-to-serialize UserFlowResult.
//
// No error return: this function is pure Go with no I/O. It always returns a
// non-nil result. On empty input it returns an empty but valid UserFlowResult.
func GroupUserFlowResult(_ context.Context, rows []UserFlowRow, maxNodes, maxLinks int) *insightsv1.UserFlowResult {
	empty := &insightsv1.UserFlowResult{}
	if len(rows) == 0 {
		return empty
	}

	nodeWeight := map[string]int64{}
	for _, row := range rows {
		nodeWeight[row.Source] += row.Value
		nodeWeight[row.Target] += row.Value
	}

	nodeIDs := slices.Collect(maps.Keys(nodeWeight))
	sort.Slice(nodeIDs, func(i, j int) bool {
		wi, wj := nodeWeight[nodeIDs[i]], nodeWeight[nodeIDs[j]]
		if wi != wj {
			return wi > wj
		}
		return nodeIDs[i] < nodeIDs[j]
	})

	topSet := map[string]struct{}{}
	if maxNodes > 0 && len(nodeIDs) > maxNodes {
		for _, id := range nodeIDs[:maxNodes] {
			topSet[id] = struct{}{}
		}
	} else {
		for _, id := range nodeIDs {
			topSet[id] = struct{}{}
		}
	}

	remap := func(id string) string {
		if _, ok := topSet[id]; ok {
			return id
		}
		return userFlowOthersNodeID
	}

	aggregated := map[[2]string]int64{}
	for _, row := range rows {
		src := remap(row.Source)
		tgt := remap(row.Target)
		if src == tgt {
			continue
		}
		key := [2]string{src, tgt}
		aggregated[key] += row.Value
	}

	type linkKey struct {
		source string
		target string
		value  int64
	}
	links := make([]linkKey, 0, len(aggregated))
	for key, value := range aggregated {
		if value <= 0 {
			continue
		}
		links = append(links, linkKey{source: key[0], target: key[1], value: value})
	}

	sort.Slice(links, func(i, j int) bool {
		if links[i].value != links[j].value {
			return links[i].value > links[j].value
		}
		if links[i].source != links[j].source {
			return links[i].source < links[j].source
		}
		return links[i].target < links[j].target
	})

	if maxLinks > 0 && len(links) > maxLinks {
		links = links[:maxLinks]
	}

	if len(links) == 0 {
		return empty
	}

	nodeSet := map[string]struct{}{}
	protoLinks := make([]*insightsv1.UserFlowLink, 0, len(links))
	for _, l := range links {
		protoLinks = append(protoLinks, &insightsv1.UserFlowLink{
			Source: proto.String(l.source),
			Target: proto.String(l.target),
			Value:  proto.Int64(l.value),
		})
		nodeSet[l.source] = struct{}{}
		nodeSet[l.target] = struct{}{}
	}

	regularNodes := make([]string, 0, len(nodeSet))
	hasOthers := false
	for id := range nodeSet {
		if id == userFlowOthersNodeID {
			hasOthers = true
			continue
		}
		regularNodes = append(regularNodes, id)
	}
	sort.Strings(regularNodes)

	protoNodes := make([]*insightsv1.UserFlowNode, 0, len(regularNodes)+1)
	for _, id := range regularNodes {
		protoNodes = append(protoNodes, &insightsv1.UserFlowNode{Id: proto.String(id), Label: proto.String(id)})
	}
	if hasOthers {
		protoNodes = append(protoNodes, &insightsv1.UserFlowNode{
			Id:    proto.String(userFlowOthersNodeID),
			Label: proto.String(userFlowOthersNodeID),
		})
	}

	return &insightsv1.UserFlowResult{Nodes: protoNodes, Links: protoLinks}
}
