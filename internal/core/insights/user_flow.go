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
			cookielessExclusionCond(excludeCookielessForPersons(req.GetSpec()), ""),
			scopeCond,
			topLevelFilterCond,
			userFlowNonEmptySessionKeyCond(),
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

// nodeRef is the internal identity of a flow-graph node. The synthetic overflow
// bucket (others=true) is a DISTINCT identity from every real node — including a
// real node whose id is literally userFlowOthersNodeID — so collapsing pruned
// nodes never merges their traffic into, or steals it from, a real "$others"
// node. Real nodes carry others=false.
type nodeRef struct {
	id     string
	others bool
}

// GroupUserFlowResult applies top-N pruning, $others collapse, and link truncation
// to raw ClickHouse rows and returns a ready-to-serialize UserFlowResult.
//
// The overflow bucket is tracked as a distinct identity (see nodeRef) and reported
// to clients via UserFlowNode.is_others — never by matching the id string. Its
// emitted id is normally userFlowOthersNodeID, but is disambiguated when a real
// surviving node already uses that id so emitted link endpoints stay unambiguous.
//
// No error return: this function is pure Go with no I/O. It always returns a
// non-nil result. On empty input it returns an empty but valid UserFlowResult.
func GroupUserFlowResult(_ context.Context, rows []UserFlowRow, maxNodes, maxLinks int) *insightsv1.UserFlowResult {
	empty := &insightsv1.UserFlowResult{}
	if len(rows) == 0 {
		return empty
	}

	// Weight each real node by total edge value touching it, from the raw rows
	// (before any collapse). The bucket is synthetic and never weighted here.
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

	// remap routes each raw id to a node identity: kept ids keep their own
	// identity (others=false); pruned ids collapse into the single synthetic
	// bucket, which stays distinct from a real node of the same id.
	bucket := nodeRef{id: userFlowOthersNodeID, others: true}
	remap := func(id string) nodeRef {
		if _, ok := topSet[id]; ok {
			return nodeRef{id: id}
		}
		return bucket
	}

	aggregated := map[[2]nodeRef]int64{}
	for _, row := range rows {
		src := remap(row.Source)
		tgt := remap(row.Target)
		if src == tgt {
			continue
		}
		aggregated[[2]nodeRef{src, tgt}] += row.Value
	}

	type flowLink struct {
		source nodeRef
		target nodeRef
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
		if links[i].source.id != links[j].source.id {
			return links[i].source.id < links[j].source.id
		}
		if links[i].source.others != links[j].source.others {
			return !links[i].source.others // real before bucket
		}
		if links[i].target.id != links[j].target.id {
			return links[i].target.id < links[j].target.id
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
	nodeSet := map[nodeRef]struct{}{}
	for _, l := range links {
		nodeSet[l.source] = struct{}{}
		nodeSet[l.target] = struct{}{}
	}

	// Resolve the bucket's emitted id. Normally userFlowOthersNodeID; if a real
	// surviving node already uses that id, append separators until unique so
	// emitted link endpoints stay unambiguous. is_others is the authoritative
	// signal regardless of the id.
	bucketID := userFlowOthersNodeID
	if _, ok := nodeSet[bucket]; ok {
		realIDs := map[string]struct{}{}
		for ref := range nodeSet {
			if !ref.others {
				realIDs[ref.id] = struct{}{}
			}
		}
		for {
			if _, clash := realIDs[bucketID]; !clash {
				break
			}
			bucketID += "_"
		}
	}
	idOf := func(ref nodeRef) string {
		if ref.others {
			return bucketID
		}
		return ref.id
	}

	protoLinks := make([]*insightsv1.UserFlowLink, 0, len(links))
	for _, l := range links {
		protoLinks = append(protoLinks, &insightsv1.UserFlowLink{
			Source: proto.String(idOf(l.source)),
			Target: proto.String(idOf(l.target)),
			Value:  proto.Int64(l.value),
		})
	}

	// Emit real nodes sorted by id, then the bucket (if present) last.
	regularIDs := make([]string, 0, len(nodeSet))
	hasOthers := false
	for ref := range nodeSet {
		if ref.others {
			hasOthers = true
			continue
		}
		regularIDs = append(regularIDs, ref.id)
	}
	sort.Strings(regularIDs)

	protoNodes := make([]*insightsv1.UserFlowNode, 0, len(regularIDs)+1)
	for _, id := range regularIDs {
		protoNodes = append(protoNodes, &insightsv1.UserFlowNode{Id: proto.String(id)})
	}
	if hasOthers {
		protoNodes = append(protoNodes, &insightsv1.UserFlowNode{
			Id:       proto.String(bucketID),
			IsOthers: proto.Bool(true),
		})
	}

	return &insightsv1.UserFlowResult{Nodes: protoNodes, Links: protoLinks}
}
