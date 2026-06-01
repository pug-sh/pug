package insights_test

import (
	"context"
	"testing"

	"github.com/pug-sh/pug/internal/core/insights"
)

func TestGroupUserFlowResult_Empty(t *testing.T) {
	ctx := context.Background()
	got := insights.GroupUserFlowResult(ctx, nil, 20, 100)
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	if len(got.GetNodes()) != 0 || len(got.GetLinks()) != 0 {
		t.Fatalf("expected empty result, got nodes=%d links=%d", len(got.GetNodes()), len(got.GetLinks()))
	}
}

func TestGroupUserFlowResult_Passthrough(t *testing.T) {
	ctx := context.Background()
	rows := []insights.UserFlowRow{
		{Source: "login", Target: "dashboard", Value: 2},
		{Source: "login", Target: "logout", Value: 1},
		{Source: "dashboard", Target: "settings", Value: 1},
		{Source: "dashboard", Target: "logout", Value: 1},
		{Source: "settings", Target: "logout", Value: 1},
	}
	got := insights.GroupUserFlowResult(ctx, rows, 20, 100)
	if len(got.GetLinks()) != 5 {
		t.Fatalf("expected 5 links, got %d", len(got.GetLinks()))
	}
	linkMap := map[[2]string]int64{}
	for _, l := range got.GetLinks() {
		linkMap[[2]string{l.GetSource(), l.GetTarget()}] = l.GetValue()
	}
	want := map[[2]string]int64{
		{"login", "dashboard"}:    2,
		{"login", "logout"}:       1,
		{"dashboard", "settings"}: 1,
		{"dashboard", "logout"}:   1,
		{"settings", "logout"}:    1,
	}
	for k, v := range want {
		if linkMap[k] != v {
			t.Errorf("link %v: got %d want %d", k, linkMap[k], v)
		}
	}
}

func TestGroupUserFlowResult_OthersCollapseRemovesSelfLoop(t *testing.T) {
	ctx := context.Background()
	rows := []insights.UserFlowRow{
		{Source: "a", Target: "b", Value: 10},
		{Source: "c", Target: "d", Value: 1},
		{Source: "e", Target: "f", Value: 1},
	}
	got := insights.GroupUserFlowResult(ctx, rows, 2, 100)
	for _, l := range got.GetLinks() {
		if l.GetSource() == l.GetTarget() {
			t.Fatalf("unexpected self-loop: %s -> %s", l.GetSource(), l.GetTarget())
		}
	}
	for _, l := range got.GetLinks() {
		if l.GetSource() == "$others" || l.GetTarget() == "$others" {
			if l.GetSource() == l.GetTarget() {
				t.Fatal("expected no $others self-loops")
			}
		}
	}
}

func TestGroupUserFlowResult_MaxLinksTruncation(t *testing.T) {
	ctx := context.Background()
	rows := []insights.UserFlowRow{
		{Source: "a", Target: "b", Value: 3},
		{Source: "a", Target: "c", Value: 2},
		{Source: "b", Target: "d", Value: 1},
	}
	got := insights.GroupUserFlowResult(ctx, rows, 20, 2)
	if len(got.GetLinks()) != 2 {
		t.Fatalf("expected 2 links, got %d", len(got.GetLinks()))
	}
	if got.GetLinks()[0].GetValue() != 3 || got.GetLinks()[1].GetValue() != 2 {
		t.Fatalf("expected highest-value links retained, got %+v", got.GetLinks())
	}
	nodeIDs := map[string]struct{}{}
	for _, n := range got.GetNodes() {
		nodeIDs[n.GetId()] = struct{}{}
	}
	if _, ok := nodeIDs["d"]; ok {
		t.Error("node d should be excluded — only referenced by truncated link")
	}
}

func TestGroupUserFlowResult_NonTopLinkDroppedAsSelfLoop(t *testing.T) {
	ctx := context.Background()
	rows := []insights.UserFlowRow{
		{Source: "heavy", Target: "x", Value: 10},
		{Source: "y", Target: "z", Value: 1},
	}
	got := insights.GroupUserFlowResult(ctx, rows, 1, 100)
	for _, l := range got.GetLinks() {
		if l.GetSource() == "$others" && l.GetTarget() == "$others" {
			t.Fatal("expected no $others self-loops in response")
		}
	}
	if len(got.GetLinks()) != 1 {
		t.Fatalf("expected 1 surviving link, got %d", len(got.GetLinks()))
	}
	if got.GetLinks()[0].GetSource() != "heavy" {
		t.Fatalf("expected heavy->* link, got %s->%s", got.GetLinks()[0].GetSource(), got.GetLinks()[0].GetTarget())
	}
}

func TestGroupUserFlowResult_OthersNodeLast(t *testing.T) {
	ctx := context.Background()
	rows := []insights.UserFlowRow{
		{Source: "keep", Target: "also", Value: 5},
		{Source: "drop", Target: "also", Value: 1},
	}
	got := insights.GroupUserFlowResult(ctx, rows, 2, 100)
	nodes := got.GetNodes()
	if len(nodes) < 2 {
		t.Fatalf("expected at least 2 nodes, got %d", len(nodes))
	}
	last := nodes[len(nodes)-1]
	if last.GetId() != "$others" {
		t.Fatalf("expected $others last, got %q", last.GetId())
	}
}

func TestGroupUserFlowResult_DeterministicOrdering(t *testing.T) {
	ctx := context.Background()
	rows := []insights.UserFlowRow{
		{Source: "b", Target: "c", Value: 2},
		{Source: "a", Target: "b", Value: 2},
	}
	got1 := insights.GroupUserFlowResult(ctx, rows, 20, 100)
	got2 := insights.GroupUserFlowResult(ctx, rows, 20, 100)
	if len(got1.GetLinks()) != len(got2.GetLinks()) {
		t.Fatal("non-deterministic link count")
	}
	for i := range got1.GetLinks() {
		l1, l2 := got1.GetLinks()[i], got2.GetLinks()[i]
		if l1.GetSource() != l2.GetSource() || l1.GetTarget() != l2.GetTarget() || l1.GetValue() != l2.GetValue() {
			t.Fatalf("non-deterministic links at %d: %+v vs %+v", i, l1, l2)
		}
	}
}

// TestGroupUserFlowResult_RealOthersNodeDoesNotMergeWithBucket pins the structural
// dedup: a real node whose id is literally "$others" must NOT have its traffic
// merged into (or stolen by) the synthetic overflow bucket created when pruned
// nodes collapse. The bucket is identified by is_others, and emitted links to the
// real node and to the bucket stay distinct.
func TestGroupUserFlowResult_RealOthersNodeDoesNotMergeWithBucket(t *testing.T) {
	ctx := context.Background()
	rows := []insights.UserFlowRow{
		{Source: "$others", Target: "home", Value: 50}, // real "$others" node
		{Source: "rare1", Target: "home", Value: 1},    // pruned → bucket
		{Source: "rare2", Target: "home", Value: 1},    // pruned → bucket
		{Source: "home", Target: "end", Value: 40},
	}
	// maxNodes=3 keeps {home, "$others"(real), end}; rare1/rare2 collapse to the bucket.
	got := insights.GroupUserFlowResult(ctx, rows, 3, 100)

	// Exactly one synthetic bucket (is_others=true) and one real "$others" (is_others=false).
	var bucketID string
	bucketCount, realOthersCount := 0, 0
	for _, n := range got.GetNodes() {
		if n.GetIsOthers() {
			bucketCount++
			bucketID = n.GetId()
		} else if n.GetId() == "$others" {
			realOthersCount++
		}
	}
	if bucketCount != 1 {
		t.Fatalf("expected exactly 1 is_others bucket node, got %d", bucketCount)
	}
	if realOthersCount != 1 {
		t.Fatalf("expected the real \"$others\" node preserved (is_others=false), got %d", realOthersCount)
	}

	links := map[[2]string]int64{}
	for _, l := range got.GetLinks() {
		links[[2]string{l.GetSource(), l.GetTarget()}] = l.GetValue()
	}
	// The real node's edge keeps its own value — NOT summed with the pruned traffic.
	if links[[2]string{"$others", "home"}] != 50 {
		t.Errorf("real $others->home: got %d want 50 (must not absorb pruned traffic)", links[[2]string{"$others", "home"}])
	}
	// The bucket's edge carries the summed pruned traffic (1+1) under its own id.
	if links[[2]string{bucketID, "home"}] != 2 {
		t.Errorf("bucket(%q)->home: got %d want 2 (rare1+rare2)", bucketID, links[[2]string{bucketID, "home"}])
	}
	if bucketID == "$others" {
		t.Errorf("bucket id should be disambiguated from the real $others node, got %q", bucketID)
	}
}

// TestGroupUserFlowResult_OthersSummation pins that multiple pruned nodes pointing
// at the same kept target sum into a single bucket link (the +=, not overwrite).
func TestGroupUserFlowResult_OthersSummation(t *testing.T) {
	ctx := context.Background()
	rows := []insights.UserFlowRow{
		{Source: "top", Target: "x", Value: 10},
		{Source: "c", Target: "x", Value: 3}, // c, d pruned (maxNodes=2 keeps top, x)
		{Source: "d", Target: "x", Value: 2},
	}
	got := insights.GroupUserFlowResult(ctx, rows, 2, 100)

	var bucketID string
	for _, n := range got.GetNodes() {
		if n.GetIsOthers() {
			bucketID = n.GetId()
		}
	}
	if bucketID == "" {
		t.Fatal("expected an is_others bucket node")
	}
	links := map[[2]string]int64{}
	for _, l := range got.GetLinks() {
		links[[2]string{l.GetSource(), l.GetTarget()}] = l.GetValue()
	}
	if got := links[[2]string{bucketID, "x"}]; got != 5 {
		t.Errorf("bucket->x: got %d want 5 (3+2 summed)", got)
	}
	if got := links[[2]string{"top", "x"}]; got != 10 {
		t.Errorf("top->x: got %d want 10", got)
	}
}
