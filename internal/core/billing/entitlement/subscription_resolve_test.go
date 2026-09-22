package entitlement_test

import (
	"testing"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
	"github.com/pug-sh/pug/internal/core/billing/entitlement"
)

func liveSub(slug string) *entitlement.Subscription {
	return &entitlement.Subscription{
		PlanSlug: slug, Status: corebilling.SubStatusActive,
		PriceCents: 2_000, Currency: "USD",
		ProviderCustomerID: "cus_1", ProviderSubID: "sub_1",
		CurrentPeriodEnd: later.AddDate(0, 0, 12),
	}
}

// Somebody is paying for this, so it outranks everything beneath it.
func TestSubscriptionSuppliesThePlan(t *testing.T) {
	ent := entitlement.Resolve(created, entitlement.Record{}, liveSub(entitlement.CurrentCard().Slug), later, true)
	if ent.Status != entitlement.StatusActive {
		t.Errorf("status = %s, want ACTIVE", ent.Status)
	}
	if ent.Slug != entitlement.CurrentCard().Slug {
		t.Errorf("slug = %q, want growth", ent.Slug)
	}
	if got, want := quota(t, ent), entitlement.CurrentCard().FreeEvents; got != want {
		t.Errorf("allowance = %d, want the card's %d", got, want)
	}
	if ent.SubStatus != corebilling.SubStatusActive {
		t.Errorf("sub_status = %q, want active", ent.SubStatus)
	}
	if ent.ProviderCustomerID != "cus_1" {
		t.Errorf("provider_customer_id = %q, want cus_1", ent.ProviderCustomerID)
	}
}

// The subscription beats an operator grant naming a different tier: the grant is
// rule 2 and only applies when nothing is being charged.
func TestSubscriptionOutranksAnOperatorGrant(t *testing.T) {
	// A grant naming a card the catalog dropped: the live subscription still wins.
	rec := entitlement.Record{Present: true, PlanSlug: "starter"}
	ent := entitlement.Resolve(created, rec, liveSub(entitlement.CurrentCard().Slug), later, true)
	if ent.Slug != entitlement.CurrentCard().Slug {
		t.Errorf("slug = %q, want scale (the subscription's, not the grant's)", ent.Slug)
	}
}

// past_due is live on purpose: the card failed, the entitlement did not.
func TestPastDueKeepsTheQuota(t *testing.T) {
	sub := liveSub(entitlement.CurrentCard().Slug)
	sub.Status = corebilling.SubStatusPastDue
	ent := entitlement.Resolve(created, entitlement.Record{}, sub, later, true)
	if ent.Slug != entitlement.CurrentCard().Slug {
		t.Errorf("slug = %q, want growth — a failed card must not degrade the plan", ent.Slug)
	}
	if got, want := quota(t, ent), entitlement.CurrentCard().FreeEvents; got != want {
		t.Errorf("allowance = %d, want the card's %d", got, want)
	}
}

// Every not-live status supplies nothing and the org falls to what is beneath.
func TestNotLiveSubscriptionSuppliesNothing(t *testing.T) {
	for _, status := range corebilling.AllSubStatuses() {
		if status.Live() {
			continue
		}
		sub := liveSub(entitlement.CurrentCard().Slug)
		sub.Status = status
		ent := entitlement.Resolve(created, entitlement.Record{}, sub, later, true)
		// "Free" is a STATUS now, not a slug: an org with nothing live falls to the
		// current card, whose allowance is what free means.
		if ent.Slug != entitlement.CurrentCard().Slug {
			t.Errorf("%s subscription resolved to %q, want the current card", status, ent.Slug)
		}
		if ent.Status != entitlement.StatusFree {
			t.Errorf("%s subscription resolved to status %s, want FREE", status, ent.Status)
		}
		if ent.SubStatus != "" {
			t.Errorf("%s subscription reported sub_status %q, want empty", status, ent.SubStatus)
		}
	}
}

// A cancelled subscription must not shadow a grant the operator made after it.
func TestCancelledSubscriptionFallsBackToTheGrant(t *testing.T) {
	sub := liveSub("starter")
	sub.Status = corebilling.SubStatusCancelled
	rec := entitlement.Record{Present: true, PlanSlug: entitlement.CurrentCard().Slug}
	ent := entitlement.Resolve(created, rec, sub, later, true)
	if ent.Slug != entitlement.CurrentCard().Slug {
		t.Errorf("slug = %q, want growth (the grant beneath a dead subscription)", ent.Slug)
	}
}

// Rule 3: the deal's quota is pug's, and the subscription is what makes the
// custom tier mean anything.
func TestCustomSubscriptionTakesItsQuotaFromTheRow(t *testing.T) {
	rec := entitlement.Record{
		Present: true, PlanSlug: "custom",
		IncludedEventsOverride: 5_000_000,
		DisplayNameOverride:    "Acme Enterprise",
		ProviderProductID:      "prod_acme",
	}
	ent := entitlement.Resolve(created, rec, liveSub("custom"), later, true)
	if got := quota(t, ent); got != 5_000_000 {
		t.Errorf("quota = %d, want 5000000", got)
	}
	if ent.DisplayName != "Acme Enterprise" {
		t.Errorf("display_name = %q, want Acme Enterprise", ent.DisplayName)
	}
	if ent.PriceCents != nil {
		t.Errorf("price_cents = %d, want absent — a deal's price lives in the provider", *ent.PriceCents)
	}
}

// A paid custom subscription with no quota row behind it. The free floor is the
// honest answer, and reconcile reports it; silently unlimited is the hazard.
func TestCustomSubscriptionWithNoQuotaFallsToFree(t *testing.T) {
	ent := entitlement.Resolve(created, entitlement.Record{}, liveSub("custom"), later, true)
	// The current card is the backstop, and its allowance is what free means.
	if ent.Slug != entitlement.CurrentCard().Slug {
		t.Errorf("slug = %q, want the current card", ent.Slug)
	}
	// The subscription is live and being charged, so the status is ACTIVE even
	// though the deal behind it has no terms; the card bounds what they get.
	if ent.Status != entitlement.StatusActive {
		t.Errorf("status = %s, want ACTIVE", ent.Status)
	}
	if got, want := quota(t, ent), entitlement.CurrentCard().FreeEvents; got != want {
		t.Errorf("allowance = %d, want the card's %d", got, want)
	}
}

// A contract bounds an operator's grant. It cannot expire a subscription the
// provider still says is live, or a live custom deal loses its quota mid-term.
func TestLapsedContractDoesNotStripALiveDealsQuota(t *testing.T) {
	rec := entitlement.Record{
		Present: true, PlanSlug: "custom",
		IncludedEventsOverride: 5_000_000,
		ContractEndsAt:         later.AddDate(0, 0, -1),
	}
	ent := entitlement.Resolve(created, rec, liveSub("custom"), later, true)
	if got := quota(t, ent); got != 5_000_000 {
		t.Errorf("quota = %d, want 5000000 — the customer is still being charged", got)
	}

	// With nothing being charged, the lapsed contract does expire the deal.
	lapsed := entitlement.Resolve(created, rec, nil, later, true)
	if lapsed.Slug != entitlement.CurrentCard().Slug {
		t.Errorf("slug with no subscription = %q, want the current card", lapsed.Slug)
	}
	if lapsed.Status != entitlement.StatusFree {
		t.Errorf("status with no subscription = %s, want FREE", lapsed.Status)
	}
}

// A catalog tier is a different purchase, so a lapsed grant's quota and name must
// not ride along on it — the customer would pay Starter for the pilot's quota.
func TestLapsedContractDoesNotRideOnACatalogSubscription(t *testing.T) {
	rec := entitlement.Record{
		Present: true, PlanSlug: "custom",
		IncludedEventsOverride: 5_000_000,
		DisplayNameOverride:    "Acme Pilot",
		ContractEndsAt:         later.AddDate(0, 0, -1),
	}
	// The subscription names a card the catalog dropped: it keeps its own name and
	// gets no allowance, and the lapsed pilot's numbers must not ride along on it.
	ent := entitlement.Resolve(created, rec, liveSub("starter"), later, true)
	if ent.IncludedEvents != nil {
		t.Errorf("allowance = %d, want none — the pilot's grant lapsed", *ent.IncludedEvents)
	}
	if ent.DisplayName != "starter" {
		t.Errorf("display name = %q, want the subscription's own slug", ent.DisplayName)
	}
}

// A slug the catalog no longer knows keeps its own name and no quota. Resolving
// it to "free, 10,000" would tell a paying customer they are over their limit.
func TestSubscriptionOnAnUnknownSlugKeepsItsName(t *testing.T) {
	ent := entitlement.Resolve(created, entitlement.Record{}, liveSub("growth-v0"), later, true)
	if ent.Slug != "growth-v0" {
		t.Errorf("slug = %q, want growth-v0", ent.Slug)
	}
	if ent.Status != entitlement.StatusActive {
		t.Errorf("status = %s, want ACTIVE", ent.Status)
	}
	if ent.IncludedEvents != nil {
		t.Errorf("quota = %d, want absent", *ent.IncludedEvents)
	}
}

// Billing off is the self-hosted shape: no quota, and no subscription consulted.
func TestSubscriptionIgnoredWhenBillingIsOff(t *testing.T) {
	ent := entitlement.Resolve(created, entitlement.Record{}, liveSub(entitlement.CurrentCard().Slug), later, false)
	if ent.Slug != "free" || ent.IncludedEvents != nil {
		t.Errorf("slug = %q quota = %v, want free with no quota", ent.Slug, ent.IncludedEvents)
	}
	if ent.SubStatus != "" {
		t.Errorf("sub_status = %q, want empty", ent.SubStatus)
	}
}

// The quota window is the org's anniversary; the subscription's period is when
// the provider bills. They are different questions and must not be conflated.
func TestSubscriptionPeriodIsNotTheQuotaWindow(t *testing.T) {
	sub := liveSub(entitlement.CurrentCard().Slug)
	sub.CurrentPeriodEnd = time.Date(2026, 6, 28, 9, 30, 0, 0, time.UTC)
	ent := entitlement.Resolve(created, entitlement.Record{}, sub, later, true)
	if ent.PeriodEnd.Equal(ent.SubPeriodEnd) {
		t.Fatal("quota window end equals the billing date; they are different questions")
	}
	if !ent.SubPeriodEnd.Equal(sub.CurrentPeriodEnd) {
		t.Errorf("sub_period_end = %s, want %s", ent.SubPeriodEnd, sub.CurrentPeriodEnd)
	}
}
