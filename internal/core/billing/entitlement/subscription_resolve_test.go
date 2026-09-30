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
		PriceCents: 100, Currency: "USD",
		ProviderCustomerID: "cus_1", ProviderSubID: "sub_1",
		CurrentPeriodEnd: later.AddDate(0, 0, 12),
	}
}

// Somebody is paying for this, so it outranks everything beneath it.
func TestSubscriptionSuppliesThePlan(t *testing.T) {
	ent := entitlement.Resolve(created, entitlement.Record{}, liveSub(entitlement.SlugUsage), later, true)
	if ent.Status != entitlement.StatusActive {
		t.Errorf("status = %s, want ACTIVE", ent.Status)
	}
	if ent.Slug != entitlement.SlugUsage {
		t.Errorf("slug = %q, want %s", ent.Slug, entitlement.SlugUsage)
	}
	if got := quota(t, ent); got != freeEvents() {
		t.Errorf("allowance = %d, want %d", got, freeEvents())
	}
	if ent.SubStatus != corebilling.SubStatusActive {
		t.Errorf("sub_status = %q, want active", ent.SubStatus)
	}
	if ent.ProviderCustomerID != "cus_1" {
		t.Errorf("provider_customer_id = %q, want cus_1", ent.ProviderCustomerID)
	}
}

// The subscription decides the plan: the row only ever names free or a deal.
func TestSubscriptionOutranksTheRow(t *testing.T) {
	rec := entitlement.Record{Present: true, PlanSlug: entitlement.SlugCustom, ProviderProductID: "prod_deal"}
	ent := entitlement.Resolve(created, rec, liveSub(entitlement.SlugUsage), later, true)
	if ent.Slug != entitlement.SlugUsage {
		t.Errorf("slug = %q, want %s (the subscription's, not the row's)", ent.Slug, entitlement.SlugUsage)
	}
}

// past_due is live on purpose: the card failed, the entitlement did not.
func TestPastDueKeepsTheQuota(t *testing.T) {
	sub := liveSub(entitlement.SlugUsage)
	sub.Status = corebilling.SubStatusPastDue
	ent := entitlement.Resolve(created, entitlement.Record{}, sub, later, true)
	if ent.Slug != entitlement.SlugUsage {
		t.Errorf("slug = %q, want %s — a failed card must not degrade the plan", ent.Slug, entitlement.SlugUsage)
	}
	if got := quota(t, ent); got != freeEvents() {
		t.Errorf("allowance = %d, want %d", got, freeEvents())
	}
}

// Every not-live status supplies nothing and the org falls to what is beneath.
func TestNotLiveSubscriptionSuppliesNothing(t *testing.T) {
	for _, status := range corebilling.AllSubStatuses() {
		if status.Live() {
			continue
		}
		sub := liveSub(entitlement.SlugUsage)
		sub.Status = status
		ent := entitlement.Resolve(created, entitlement.Record{}, sub, later, true)
		if ent.Slug != entitlement.SlugFree {
			t.Errorf("%s subscription resolved to %q, want free", status, ent.Slug)
		}
		if ent.SubStatus != "" {
			t.Errorf("%s subscription reported sub_status %q, want empty", status, ent.SubStatus)
		}
	}
}

// A cancelled subscription must not shadow a comp the operator made after it.
func TestCancelledSubscriptionFallsBackToTheComp(t *testing.T) {
	sub := liveSub(entitlement.SlugUsage)
	sub.Status = corebilling.SubStatusCancelled
	rec := entitlement.Record{
		Present: true, PlanSlug: entitlement.SlugFree,
		IncludedEventsOverride: 5_000_000, ContractEndsAt: later.AddDate(0, 1, 0),
	}
	ent := entitlement.Resolve(created, rec, sub, later, true)
	if ent.Status != entitlement.StatusFree || quota(t, ent) != 5_000_000 {
		t.Errorf("resolved %s with %d, want FREE with the comp's 5000000 beneath a dead subscription",
			ent.Status, quota(t, ent))
	}
}

// The deal's allowance is pug's, and the subscription is what makes the custom
// tier mean anything.
func TestCustomSubscriptionTakesItsQuotaFromTheRow(t *testing.T) {
	rec := entitlement.Record{
		Present: true, PlanSlug: entitlement.SlugCustom, BasePlanSlug: entitlement.SlugUsage,
		IncludedEventsOverride: 5_000_000,
		DisplayNameOverride:    "Acme Enterprise",
		ProviderProductID:      "prod_acme",
	}
	ent := entitlement.Resolve(created, rec, liveSub(entitlement.SlugCustom), later, true)
	if got := quota(t, ent); got != 5_000_000 {
		t.Errorf("allowance = %d, want 5000000", got)
	}
	if ent.DisplayName != "Acme Enterprise" {
		t.Errorf("display_name = %q, want Acme Enterprise", ent.DisplayName)
	}
}

// A contract bounds an operator's grant. It cannot expire a subscription the
// provider still says is live, or a live custom deal loses its allowance mid-term.
func TestLapsedContractDoesNotStripALiveDealsQuota(t *testing.T) {
	rec := entitlement.Record{
		Present: true, PlanSlug: entitlement.SlugCustom, ProviderProductID: "prod_acme",
		BasePlanSlug:           entitlement.SlugUsage,
		IncludedEventsOverride: 5_000_000,
		ContractEndsAt:         later.AddDate(0, 0, -1),
	}
	ent := entitlement.Resolve(created, rec, liveSub(entitlement.SlugCustom), later, true)
	if got := quota(t, ent); got != 5_000_000 {
		t.Errorf("allowance = %d, want 5000000 — the customer is still being charged", got)
	}

	// With nothing being charged, the deal's terms are gone with it.
	lapsed := entitlement.Resolve(created, rec, nil, later, true)
	if lapsed.Slug != entitlement.SlugFree || quota(t, lapsed) != freeEvents() {
		t.Errorf("with no subscription: %s on %d, want free on %d", lapsed.Slug, quota(t, lapsed), freeEvents())
	}
}

// A staged deal is not held until its subscription lands: its terms are what the
// org would buy, so the org stays plain free, name and all.
func TestAStagedDealIsPlainFree(t *testing.T) {
	rec := entitlement.Record{
		Present: true, PlanSlug: entitlement.SlugCustom, ProviderProductID: "prod_acme",
		BasePlanSlug:           entitlement.SlugUsage,
		IncludedEventsOverride: 5_000_000,
		RetentionDaysOverride:  2555,
		DisplayNameOverride:    "Acme Enterprise",
	}
	ent := entitlement.Resolve(created, rec, nil, later, true)
	if ent.Status != entitlement.StatusFree || ent.DisplayName != entitlement.FreeDisplayName ||
		quota(t, ent) != freeEvents() || retention(t, ent) != freeRetention() {
		t.Errorf("resolved %s %q on %v / %v, want plain free", ent.Status, ent.DisplayName, str(ent.IncludedEvents), str(ent.RetentionDays))
	}
}

// Buying the usage plan instead of a staged deal is a different purchase: the deal's
// terms must not ride on it, lapsed or not, or its allowance goes unbilled at usage
// rates.
func TestADealsTermsDoNotRideOnAPublicPlan(t *testing.T) {
	rec := entitlement.Record{
		Present: true, PlanSlug: entitlement.SlugCustom, ProviderProductID: "prod_acme",
		BasePlanSlug:           entitlement.SlugUsage,
		IncludedEventsOverride: 5_000_000,
		DisplayNameOverride:    "Acme Enterprise",
		ContractEndsAt:         later.AddDate(1, 0, 0),
	}
	ent := entitlement.Resolve(created, rec, liveSub(entitlement.SlugUsage), later, true)
	if got := quota(t, ent); got != freeEvents() {
		t.Errorf("allowance = %d, want the usage plan's %d", got, freeEvents())
	}
	if want := entitlement.CurrentPlan().DisplayName; ent.DisplayName != want {
		t.Errorf("display name = %q, want %q", ent.DisplayName, want)
	}
}

// The usage plan is a different purchase, so a lapsed deal's allowance and name
// must not ride along on it — the customer would pay usage rates on the pilot's
// allowance.
func TestLapsedContractDoesNotRideOnACatalogSubscription(t *testing.T) {
	rec := entitlement.Record{
		Present: true, PlanSlug: entitlement.SlugCustom, ProviderProductID: "prod_acme",
		IncludedEventsOverride: 5_000_000,
		DisplayNameOverride:    "Acme Pilot",
		ContractEndsAt:         later.AddDate(0, 0, -1),
	}
	ent := entitlement.Resolve(created, rec, liveSub(entitlement.SlugUsage), later, true)
	if got := quota(t, ent); got != freeEvents() {
		t.Errorf("allowance = %d, want %d — the pilot's grant lapsed", got, freeEvents())
	}
	if want := entitlement.CurrentPlan().DisplayName; ent.DisplayName != want {
		t.Errorf("display name = %q, want %q", ent.DisplayName, want)
	}
}

// A slug the catalog no longer knows keeps its own name and no allowance. Resolving
// it to the free allowance would tell a paying customer they are over it.
func TestSubscriptionOnAnUnknownSlugKeepsItsName(t *testing.T) {
	ent := entitlement.Resolve(created, entitlement.Record{}, liveSub("growth-v0"), later, true)
	if ent.Slug != "growth-v0" {
		t.Errorf("slug = %q, want growth-v0", ent.Slug)
	}
	if ent.Status != entitlement.StatusActive {
		t.Errorf("status = %s, want ACTIVE", ent.Status)
	}
	if ent.IncludedEvents != nil {
		t.Errorf("allowance = %d, want absent", *ent.IncludedEvents)
	}
	if ent.RetentionDays != nil {
		t.Errorf("retention = %d, want no bound", *ent.RetentionDays)
	}
}

// Billing off is the self-hosted shape: no allowance, and no subscription consulted.
func TestSubscriptionIgnoredWhenBillingIsOff(t *testing.T) {
	ent := entitlement.Resolve(created, entitlement.Record{}, liveSub(entitlement.SlugUsage), later, false)
	if ent.Slug != entitlement.SlugFree || ent.IncludedEvents != nil {
		t.Errorf("slug = %q allowance = %v, want free with none", ent.Slug, ent.IncludedEvents)
	}
	if ent.SubStatus != "" {
		t.Errorf("sub_status = %q, want empty", ent.SubStatus)
	}
}

// The quota window is the org's anniversary; the subscription's period is when
// the provider bills. They are different questions and must not be conflated.
func TestSubscriptionPeriodIsNotTheQuotaWindow(t *testing.T) {
	sub := liveSub(entitlement.SlugUsage)
	sub.CurrentPeriodStart = time.Date(2026, 5, 28, 9, 30, 0, 0, time.UTC)
	sub.CurrentPeriodEnd = time.Date(2026, 6, 28, 9, 30, 0, 0, time.UTC)
	ent := entitlement.Resolve(created, entitlement.Record{}, sub, later, true)
	if ent.PeriodEnd.Equal(ent.SubPeriodEnd) {
		t.Fatal("quota window end equals the billing date; they are different questions")
	}
	// The start is what the stated tiers are looked up by, so it must be the
	// provider's period and not the quota window's.
	if !ent.SubPeriodStart.Equal(sub.CurrentPeriodStart) || !ent.SubPeriodEnd.Equal(sub.CurrentPeriodEnd) {
		t.Errorf("sub period = [%s, %s), want [%s, %s)",
			ent.SubPeriodStart, ent.SubPeriodEnd, sub.CurrentPeriodStart, sub.CurrentPeriodEnd)
	}
}
