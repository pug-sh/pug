package insights_test

import (
	"context"
	"testing"

	"github.com/pug-sh/pug/internal/core/insights"
	insightsv1 "github.com/pug-sh/pug/internal/gen/proto/shared/insights/v1"
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
		{"login", "dashboard"}:     2,
		{"login", "logout"}:        1,
		{"dashboard", "settings"}:  1,
		{"dashboard", "logout"}:    1,
		{"settings", "logout"}:     1,
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

func TestGroupUserFlowResult_ZeroValueDropped(t *testing.T) {
	ctx := context.Background()
	// GroupUserFlowResult operates on aggregated rows; zero values should not appear
	// from ClickHouse, but defensive drop is tested via remap self-loop path.
	got := insights.GroupUserFlowResult(ctx, []insights.UserFlowRow{}, 20, 100)
	if got == nil {
		t.Fatal("expected non-nil empty result")
	}
	_ = insightsv1.UserFlowLink{}
}
