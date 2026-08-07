package insights_test

import (
	"context"
	"testing"

	"github.com/pug-sh/pug/internal/core/insights"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
)

// edge is a link resolved into (depth,label) terms so step-model assertions read
// in terms of the underlying nodes rather than opaque "depth:label" ids.
type edge struct {
	srcDepth int32
	srcLabel string
	tgtDepth int32
	tgtLabel string
	value    int64
}

func resolveEdges(res *insightsv1.UserFlowResult) []edge {
	byID := map[string]*insightsv1.UserFlowNode{}
	for _, n := range res.GetNodes() {
		byID[n.GetId()] = n
	}
	out := make([]edge, 0, len(res.GetLinks()))
	for _, l := range res.GetLinks() {
		s, t := byID[l.GetSource()], byID[l.GetTarget()]
		out = append(out, edge{
			srcDepth: s.GetDepth(), srcLabel: s.GetLabel(),
			tgtDepth: t.GetDepth(), tgtLabel: t.GetLabel(),
			value:    l.GetValue(),
		})
	}
	return out
}

func nodesByLabel(res *insightsv1.UserFlowResult, label string) []*insightsv1.UserFlowNode {
	var out []*insightsv1.UserFlowNode
	for _, n := range res.GetNodes() {
		if n.GetLabel() == label {
			out = append(out, n)
		}
	}
	return out
}

func findEdge(edges []edge, srcLabel string, srcDepth int32, tgtLabel string) (edge, bool) {
	for _, e := range edges {
		if e.srcLabel == srcLabel && e.srcDepth == srcDepth && e.tgtLabel == tgtLabel {
			return e, true
		}
	}
	return edge{}, false
}

func TestGroupUserFlowResult_Empty(t *testing.T) {
	got := insights.GroupUserFlowResult(context.Background(), nil, 20, 250)
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	if len(got.GetNodes()) != 0 || len(got.GetLinks()) != 0 {
		t.Fatalf("expected empty result, got nodes=%d links=%d", len(got.GetNodes()), len(got.GetLinks()))
	}
}

// chainRows is the four-session seed (login→dashboard→settings→logout etc.)
// already exploded into step-scoped edges, matching what the SQL emits.
func chainRows() []insights.UserFlowRow {
	return []insights.UserFlowRow{
		{Step: 0, Source: "login", Target: "dashboard", Value: 2},
		{Step: 0, Source: "login", Target: "logout", Value: 1},
		{Step: 1, Source: "dashboard", Target: "settings", Value: 1},
		{Step: 1, Source: "dashboard", Target: "logout", Value: 1},
		{Step: 2, Source: "settings", Target: "logout", Value: 1},
	}
}

func TestGroupUserFlowResult_ChainAdjacencyAndNodes(t *testing.T) {
	got := insights.GroupUserFlowResult(context.Background(), chainRows(), 20, 250)

	if len(got.GetLinks()) != 5 {
		t.Fatalf("expected 5 links, got %d", len(got.GetLinks()))
	}
	// 6 nodes: login@0, dashboard@1, logout@1, settings@2, logout@2, logout@3.
	if len(got.GetNodes()) != 6 {
		t.Fatalf("expected 6 nodes, got %d", len(got.GetNodes()))
	}

	edges := resolveEdges(got)
	for _, e := range edges {
		if e.tgtDepth != e.srcDepth+1 {
			t.Errorf("non-adjacent link %s@%d -> %s@%d (every edge must span exactly one column)",
				e.srcLabel, e.srcDepth, e.tgtLabel, e.tgtDepth)
		}
	}

	want := []struct {
		srcLabel string
		srcDepth int32
		tgtLabel string
		value    int64
	}{
		{"login", 0, "dashboard", 2},
		{"login", 0, "logout", 1},
		{"dashboard", 1, "settings", 1},
		{"dashboard", 1, "logout", 1},
		{"settings", 2, "logout", 1},
	}
	for _, w := range want {
		e, ok := findEdge(edges, w.srcLabel, w.srcDepth, w.tgtLabel)
		if !ok {
			t.Errorf("missing edge %s@%d -> %s", w.srcLabel, w.srcDepth, w.tgtLabel)
			continue
		}
		if e.value != w.value {
			t.Errorf("edge %s@%d -> %s: got %d want %d", w.srcLabel, w.srcDepth, w.tgtLabel, e.value, w.value)
		}
	}

	if logins := nodesByLabel(got, "login"); len(logins) != 1 || logins[0].GetDepth() != 0 {
		t.Errorf("expected one login node at depth 0, got %+v", logins)
	}
}

// TestGroupUserFlowResult_SameLabelDistinctPerStep pins the core of the step
// model: one label reached at several positions becomes several distinct nodes,
// one per depth — never collapsed into a single cyclic node.
func TestGroupUserFlowResult_SameLabelDistinctPerStep(t *testing.T) {
	got := insights.GroupUserFlowResult(context.Background(), chainRows(), 20, 250)

	logouts := nodesByLabel(got, "logout")
	if len(logouts) != 3 {
		t.Fatalf("expected 3 distinct logout nodes (depths 1,2,3), got %d", len(logouts))
	}
	depths := map[int32]bool{}
	ids := map[string]bool{}
	for _, n := range logouts {
		depths[n.GetDepth()] = true
		ids[n.GetId()] = true
		if n.GetIsOthers() {
			t.Errorf("logout node unexpectedly flagged is_others: %+v", n)
		}
	}
	for _, d := range []int32{1, 2, 3} {
		if !depths[d] {
			t.Errorf("expected a logout node at depth %d", d)
		}
	}
	if len(ids) != 3 {
		t.Errorf("expected 3 distinct logout node ids, got %d (%v)", len(ids), ids)
	}
}

// TestGroupUserFlowResult_ConsecutiveRepeatIsForwardStep proves a session firing
// the same event twice in a row is a real depth d → d+1 edge, not a dropped
// self-loop (the endpoints are distinct nodes at different depths).
func TestGroupUserFlowResult_ConsecutiveRepeatIsForwardStep(t *testing.T) {
	rows := []insights.UserFlowRow{{Step: 0, Source: "a", Target: "a", Value: 3}}
	got := insights.GroupUserFlowResult(context.Background(), rows, 20, 250)

	if len(got.GetLinks()) != 1 {
		t.Fatalf("expected 1 link, got %d", len(got.GetLinks()))
	}
	l := got.GetLinks()[0]
	if l.GetSource() == l.GetTarget() {
		t.Fatalf("endpoints must be distinct nodes, got self-referential id %q", l.GetSource())
	}
	edges := resolveEdges(got)
	if e := edges[0]; e.srcLabel != "a" || e.tgtLabel != "a" || e.srcDepth != 0 || e.tgtDepth != 1 {
		t.Fatalf("expected a@0 -> a@1, got %s@%d -> %s@%d", e.srcLabel, e.srcDepth, e.tgtLabel, e.tgtDepth)
	}
}

// TestGroupUserFlowResult_PerStepTopNIndependence is the crown jewel: top-N is
// per depth, so a label pruned at one step can still survive at another. "rare"
// is pruned out of the busy depth 0 but kept as the sole node at depth 2.
func TestGroupUserFlowResult_PerStepTopNIndependence(t *testing.T) {
	rows := []insights.UserFlowRow{
		{Step: 0, Source: "rare", Target: "hub", Value: 1},
		{Step: 0, Source: "big1", Target: "hub", Value: 5},
		{Step: 0, Source: "big2", Target: "hub", Value: 5},
		{Step: 1, Source: "hub", Target: "rare", Value: 9},
	}
	got := insights.GroupUserFlowResult(context.Background(), rows, 2, 250)
	edges := resolveEdges(got)

	// "rare" survived at depth 2 even though it was pruned at depth 0.
	rares := nodesByLabel(got, "rare")
	if len(rares) != 1 {
		t.Fatalf("expected exactly one surviving rare node (the depth-2 one), got %d", len(rares))
	}
	if rares[0].GetDepth() != 2 {
		t.Errorf("surviving rare node depth: got %d want 2", rares[0].GetDepth())
	}
	if e, ok := findEdge(edges, "hub", 1, "rare"); !ok || e.value != 9 || e.tgtDepth != 2 {
		t.Errorf("expected hub@1 -> rare@2 value 9, got %+v ok=%v", e, ok)
	}

	// At depth 0 the pruned "rare" collapsed into that depth's bucket.
	if e, ok := findEdge(edges, "$others", 0, "hub"); !ok || e.value != 1 {
		t.Errorf("expected $others@0 -> hub value 1 (pruned rare), got %+v ok=%v", e, ok)
	}
	bucketAt0 := false
	for _, n := range got.GetNodes() {
		if n.GetIsOthers() && n.GetDepth() == 0 {
			bucketAt0 = true
		}
	}
	if !bucketAt0 {
		t.Error("expected an is_others bucket at depth 0")
	}
}

// TestGroupUserFlowResult_PerStepBucketSummation pins that pruned nodes at a
// depth collapse and SUM into that depth's single bucket edge.
func TestGroupUserFlowResult_PerStepBucketSummation(t *testing.T) {
	rows := []insights.UserFlowRow{
		{Step: 0, Source: "hub", Target: "a", Value: 1},
		{Step: 0, Source: "hub", Target: "b", Value: 1},
		{Step: 0, Source: "hub", Target: "c", Value: 1},
		{Step: 0, Source: "hub", Target: "d", Value: 1},
	}
	got := insights.GroupUserFlowResult(context.Background(), rows, 2, 250)
	edges := resolveEdges(got)

	// depth 1 keeps a, b (label asc among equal weights); c, d collapse to bucket.
	if e, ok := findEdge(edges, "hub", 0, "a"); !ok || e.value != 1 {
		t.Errorf("expected hub@0 -> a value 1, got %+v ok=%v", e, ok)
	}
	if e, ok := findEdge(edges, "hub", 0, "$others"); !ok || e.value != 2 {
		t.Errorf("expected hub@0 -> $others value 2 (c+d summed), got %+v ok=%v", e, ok)
	}
	for _, pruned := range []string{"c", "d"} {
		if n := nodesByLabel(got, pruned); len(n) != 0 {
			t.Errorf("pruned leaf %q should not appear as a real node", pruned)
		}
	}
}

func TestGroupUserFlowResult_MaxLinksTruncation(t *testing.T) {
	rows := []insights.UserFlowRow{
		{Step: 0, Source: "a", Target: "b", Value: 3},
		{Step: 0, Source: "a", Target: "c", Value: 2},
		{Step: 1, Source: "b", Target: "d", Value: 1},
	}
	got := insights.GroupUserFlowResult(context.Background(), rows, 20, 2)
	if len(got.GetLinks()) != 2 {
		t.Fatalf("expected 2 links, got %d", len(got.GetLinks()))
	}
	if got.GetLinks()[0].GetValue() != 3 || got.GetLinks()[1].GetValue() != 2 {
		t.Fatalf("expected highest-value links retained, got %+v", got.GetLinks())
	}
	// "d" was only referenced by the truncated b->d link, so it drops out.
	if n := nodesByLabel(got, "d"); len(n) != 0 {
		t.Error("node d should be excluded — only referenced by truncated link")
	}
}

// TestGroupUserFlowResult_RealOthersDoesNotMergeWithBucket pins per-depth bucket
// distinctness: a real node literally named "$others" at the same depth as that
// depth's overflow bucket keeps its own identity, id, and traffic.
func TestGroupUserFlowResult_RealOthersDoesNotMergeWithBucket(t *testing.T) {
	rows := []insights.UserFlowRow{
		{Step: 0, Source: "$others", Target: "home", Value: 50}, // real "$others"@0
		{Step: 0, Source: "rare1", Target: "home", Value: 1},
		{Step: 0, Source: "rare2", Target: "home", Value: 1}, // pruned → bucket@0
		{Step: 0, Source: "rare3", Target: "home", Value: 1}, // pruned → bucket@0
	}
	// maxNodes=2 keeps {"$others"(50), rare1(1)} at depth 0; rare2, rare3 collapse.
	got := insights.GroupUserFlowResult(context.Background(), rows, 2, 250)

	othersLabeled := nodesByLabel(got, "$others")
	var realID, bucketID string
	realCount, bucketCount := 0, 0
	for _, n := range othersLabeled {
		if n.GetDepth() != 0 {
			t.Errorf("unexpected $others-labeled node at depth %d", n.GetDepth())
		}
		if n.GetIsOthers() {
			bucketCount++
			bucketID = n.GetId()
		} else {
			realCount++
			realID = n.GetId()
		}
	}
	if realCount != 1 || bucketCount != 1 {
		t.Fatalf("expected one real and one bucket $others-labeled node, got real=%d bucket=%d", realCount, bucketCount)
	}
	if realID == bucketID {
		t.Fatalf("real and bucket nodes must have distinct ids, both = %q", realID)
	}

	edges := resolveEdges(got)
	// Resolve by id since both endpoints share the label "$others".
	var realToHome, bucketToHome int64
	for _, l := range got.GetLinks() {
		switch l.GetSource() {
		case realID:
			realToHome = l.GetValue()
		case bucketID:
			bucketToHome = l.GetValue()
		}
	}
	if realToHome != 50 {
		t.Errorf("real $others@0 -> home: got %d want 50 (must not absorb pruned traffic)", realToHome)
	}
	if bucketToHome != 2 {
		t.Errorf("bucket@0 -> home: got %d want 2 (rare2+rare3)", bucketToHome)
	}
	_ = edges
}

// TestGroupUserFlowResult_BucketLastWithinDepth pins node ordering: within a
// depth, the synthetic bucket is emitted after every real node of that depth.
func TestGroupUserFlowResult_BucketLastWithinDepth(t *testing.T) {
	rows := []insights.UserFlowRow{
		{Step: 0, Source: "hub", Target: "a", Value: 3},
		{Step: 0, Source: "hub", Target: "b", Value: 2},
		{Step: 0, Source: "hub", Target: "c", Value: 1},
	}
	got := insights.GroupUserFlowResult(context.Background(), rows, 2, 250)

	// Track, per depth, whether a bucket has already been emitted; a real node of
	// the same depth appearing after it is an ordering violation.
	bucketSeen := map[int32]bool{}
	for _, n := range got.GetNodes() {
		d := n.GetDepth()
		if n.GetIsOthers() {
			bucketSeen[d] = true
			continue
		}
		if bucketSeen[d] {
			t.Errorf("real node %s@%d emitted after its depth's bucket", n.GetLabel(), d)
		}
	}
}

func TestGroupUserFlowResult_DeterministicOrdering(t *testing.T) {
	rows := chainRows()
	got1 := insights.GroupUserFlowResult(context.Background(), rows, 20, 250)
	got2 := insights.GroupUserFlowResult(context.Background(), rows, 20, 250)
	if len(got1.GetLinks()) != len(got2.GetLinks()) {
		t.Fatal("non-deterministic link count")
	}
	for i := range got1.GetLinks() {
		l1, l2 := got1.GetLinks()[i], got2.GetLinks()[i]
		if l1.GetSource() != l2.GetSource() || l1.GetTarget() != l2.GetTarget() || l1.GetValue() != l2.GetValue() {
			t.Fatalf("non-deterministic links at %d: %+v vs %+v", i, l1, l2)
		}
	}
	if len(got1.GetNodes()) != len(got2.GetNodes()) {
		t.Fatal("non-deterministic node count")
	}
	for i := range got1.GetNodes() {
		if got1.GetNodes()[i].GetId() != got2.GetNodes()[i].GetId() {
			t.Fatalf("non-deterministic node order at %d: %q vs %q", i, got1.GetNodes()[i].GetId(), got2.GetNodes()[i].GetId())
		}
	}
}
