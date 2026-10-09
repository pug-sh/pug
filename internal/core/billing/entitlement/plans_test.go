package entitlement_test

import (
	"slices"
	"testing"

	"github.com/pug-sh/pug/internal/core/billing/entitlement"
)

// A sold plan's numbers must match the meters on its provider product, and an edit
// re-splits every subscription on it silently. The values are PLACEHOLDERS until
// the commercial split is decided — then this pin is what makes changing them a
// deliberate act.
func TestCatalogIsPinned(t *testing.T) {
	want := map[string]struct {
		free      int64
		tierUpTo  []int64
		retention int64
		retired   bool
	}{
		entitlement.SlugUsage: {
			free:      100_000,
			tierUpTo:  []int64{2_000_000, 15_000_000, 50_000_000, 100_000_000, 250_000_000},
			retention: 1_825,
		},
	}
	plans := entitlement.Plans()
	if len(plans) != len(want) {
		t.Fatalf("catalog has %d plans, want %d", len(plans), len(want))
	}
	for _, p := range plans {
		w, ok := want[p.Slug]
		if !ok {
			t.Fatalf("unpinned plan %q", p.Slug)
		}
		if p.FreeEvents != w.free || !slices.Equal(p.TierUpTo, w.tierUpTo) ||
			p.RetentionDays != w.retention || p.Retired != w.retired {
			t.Errorf("plan %q = %+v, want %+v", p.Slug, p, w)
		}
	}
}

// Free and custom are states, never catalog entries: nothing may sell them.
func TestFreeAndCustomAreStatesNotPlans(t *testing.T) {
	for _, slug := range []string{entitlement.SlugFree, entitlement.SlugCustom} {
		if _, ok := entitlement.PlanBySlug(slug); ok {
			t.Errorf("PlanBySlug(%q) found a plan; it is a state", slug)
		}
	}
}

func TestCurrentPlanIsTheUsagePlan(t *testing.T) {
	if got := entitlement.CurrentPlan(); got.Slug != entitlement.SlugUsage || !got.OnSale() {
		t.Fatalf("CurrentPlan = %+v, want the usage plan on sale", got)
	}
	if got := entitlement.CurrentPlan().Tiers(); got != 6 {
		t.Fatalf("Tiers = %d, want 6 (five bounds and the unbounded last)", got)
	}
}

func TestTiersFor(t *testing.T) {
	current := entitlement.CurrentPlan().TierUpTo
	if got, ok := entitlement.TiersFor(entitlement.SlugUsage); !ok || !slices.Equal(got, current) {
		t.Errorf("TiersFor(usage) = %v, %v", got, ok)
	}
	// A deal splits over its own base plan, which only its row names: its layout is
	// TiersFor(that plan), never the current plan's by default.
	for _, slug := range []string{entitlement.SlugCustom, entitlement.SlugFree, "growth", ""} {
		if _, ok := entitlement.TiersFor(slug); ok {
			t.Errorf("TiersFor(%q) reported a layout; nothing can split it", slug)
		}
	}
}

// A caller writing through a returned plan must not re-split the catalog.
func TestCatalogSlicesAreNotShared(t *testing.T) {
	p, _ := entitlement.PlanBySlug(entitlement.SlugUsage)
	p.TierUpTo[0] = 1
	listed := entitlement.Plans()[0]
	listed.TierUpTo[1] = 1
	cur := entitlement.CurrentPlan()
	cur.TierUpTo[2] = 1
	tiers, _ := entitlement.TiersFor(entitlement.SlugUsage)
	tiers[3] = 1
	if again, _ := entitlement.PlanBySlug(entitlement.SlugUsage); again.TierUpTo[0] == 1 || again.TierUpTo[1] == 1 ||
		again.TierUpTo[2] == 1 || again.TierUpTo[3] == 1 {
		t.Fatalf("the catalog changed through a returned slice: %v", again.TierUpTo)
	}
}
