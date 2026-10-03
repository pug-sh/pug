package entitlement

import (
	"slices"
	"testing"
	"time"

	"github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"github.com/rs/xid"
)

// retiredPlan is appended to the catalog by the tests below: no plan is retired
// yet, and an unexercised rule stops working without anyone noticing. Appending is
// why these tests live inside the package rather than in entitlement_test.
var retiredPlan = Plan{
	Slug: "usage-2025-01", DisplayName: "Old", FreeEvents: 50_000,
	TierUpTo: []int64{1_000_000}, RetentionDays: 365, Retired: true,
}

func withRetiredPlan(t *testing.T) {
	t.Helper()
	original := catalog
	t.Cleanup(func() { catalog = original })
	catalog = append(append([]Plan(nil), original...), retiredPlan)
}

// Plans() must keep listing a retired plan: that list is what the product map is
// built from, so dropping it rejects its holders' renewals AND cancellations
// permanently — one reprice freezes every incumbent's subscription.
func TestRetiredPlanStaysInThePlanList(t *testing.T) {
	withRetiredPlan(t)
	var found bool
	for _, p := range Plans() {
		if p.Slug == retiredPlan.Slug {
			found = true
		}
	}
	if !found {
		t.Error("a retired plan is missing from Plans(), so nothing can map its product back to a slug")
	}
}

// The point of retiring rather than deleting a plan: an org already on one keeps
// resolving on its numbers, while nothing sells it and the current plan is the
// newest one on sale.
func TestRetiredPlanIsStillHeldByItsSubscribers(t *testing.T) {
	withRetiredPlan(t)
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	sub := &Subscription{PlanSlug: retiredPlan.Slug, Status: billing.SubStatusActive}
	ent := Resolve(now.AddDate(0, -3, 0), Record{}, sub, now, true)
	if ent.Status != StatusActive || ent.Slug != retiredPlan.Slug {
		t.Fatalf("resolved %s/%s, want ACTIVE on the retired plan", ent.Status, ent.Slug)
	}
	if ent.IncludedEvents == nil || *ent.IncludedEvents != retiredPlan.FreeEvents ||
		!slices.Equal(ent.TierUpTo, retiredPlan.TierUpTo) {
		t.Errorf("resolved %v / %v, want the retired plan's own allowance and tiers", ent.IncludedEvents, ent.TierUpTo)
	}
	if got, _ := PlanBySlug(retiredPlan.Slug); got.OnSale() {
		t.Error("a retired plan must not be on sale")
	}
	if got := CurrentPlan(); got.Slug == retiredPlan.Slug {
		t.Error("CurrentPlan returned the retired plan")
	}
}

// A reprice mints a plan and retires the old one. A deal pinned to the old one keeps
// splitting over it: its product carries that plan's meters, and nothing re-made it.
func TestARepriceDoesNotResplitALiveDeal(t *testing.T) {
	withRetiredPlan(t)
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	rec := Record{
		Present: true, PlanSlug: SlugCustom, ProviderProductID: "prod_deal",
		BasePlanSlug: retiredPlan.Slug,
	}
	sub := &Subscription{PlanSlug: SlugCustom, Status: billing.SubStatusActive}
	ent := Resolve(now.AddDate(0, -3, 0), rec, sub, now, true)
	if ent.Status != StatusActive || ent.Slug != SlugCustom {
		t.Fatalf("resolved %s/%s, want ACTIVE/custom", ent.Status, ent.Slug)
	}
	if ent.IncludedEvents == nil || *ent.IncludedEvents != retiredPlan.FreeEvents ||
		ent.RetentionDays == nil || *ent.RetentionDays != retiredPlan.RetentionDays ||
		!slices.Equal(ent.TierUpTo, retiredPlan.TierUpTo) {
		t.Errorf("resolved %v / %v / %v, want the pinned plan's allowance, retention and tiers",
			ent.IncludedEvents, ent.RetentionDays, ent.TierUpTo)
	}
}

// withRepricedCatalog is the catalog after a reprice: every plan retired, and a new
// one on sale.
func withRepricedCatalog(t *testing.T) Plan {
	t.Helper()
	original := catalog
	t.Cleanup(func() { catalog = original })
	next := Plan{
		Slug: "usage-2027-01", DisplayName: "New", FreeEvents: 200_000,
		TierUpTo: []int64{3_000_000}, RetentionDays: 365,
	}
	repriced := make([]Plan, 0, len(original)+1)
	for _, p := range original {
		p.Retired = true
		repriced = append(repriced, p)
	}
	catalog = append(repriced, next)
	return next
}

// A renewal on the same product keeps a deal's pin through a reprice — the product
// still carries the old plan's meters — and a new product, made against the current
// plan, moves it.
func TestADealKeepsItsPinUntilItsProductChanges(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	svc, err := NewService(pg.PgRO, pg.PgW, true)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	orgID := seedOrg(t, pg)
	ctx := t.Context()
	const actor = "tester@localhost"

	staged, err := svc.SetPlan(ctx, orgID, actor, Change{PlanSlug: SlugCustom, ProviderProductID: new("prod_deal")})
	if err != nil {
		t.Fatalf("stage deal: %v", err)
	}
	next := withRepricedCatalog(t)

	renewed, err := svc.SetPlan(ctx, orgID, actor, Change{PlanSlug: SlugCustom, Note: new("renewal")})
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if renewed.BasePlanSlug != staged.BasePlanSlug {
		t.Errorf("a renewal moved the pin from %q to %q", staged.BasePlanSlug, renewed.BasePlanSlug)
	}
	remade, err := svc.SetPlan(ctx, orgID, actor, Change{PlanSlug: SlugCustom, ProviderProductID: new("prod_deal_2027")})
	if err != nil {
		t.Fatalf("new product: %v", err)
	}
	if remade.BasePlanSlug != next.Slug {
		t.Errorf("a new product pinned %q, want the current plan %q", remade.BasePlanSlug, next.Slug)
	}
}

func seedOrg(t *testing.T, pg *testutil.TestPostgres) string {
	t.Helper()
	org, err := dbwrite.New(pg.PgW).CreateOrg(t.Context(), dbwrite.CreateOrgParams{
		ID:          xid.New().String(),
		DisplayName: "acme-" + xid.New().String(),
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	return org.ID
}
