package billing_test

import (
	"testing"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
)

func liveSub(slug string) *corebilling.Subscription {
	return &corebilling.Subscription{
		PlanSlug: slug, Status: corebilling.SubStatusActive, OnDemand: true,
		Currency: "USD", ProviderCustomerID: "cus_1", ProviderSubID: "sub_1",
		CurrentPeriodEnd: later.AddDate(0, 0, 12),
	}
}

// A live mandate is what makes an org chargeable, on the card it pinned.
func TestMandateMakesTheOrgChargeable(t *testing.T) {
	ent := corebilling.Resolve(created, corebilling.Record{}, liveSub(corebilling.CurrentSlug), later, true)
	if ent.Status != corebilling.StatusActive {
		t.Errorf("status = %s, want ACTIVE", ent.Status)
	}
	if !ent.Chargeable {
		t.Error("chargeable = false with a live on-demand mandate")
	}
	if ent.Slug != corebilling.CurrentSlug || ent.Card == nil {
		t.Errorf("slug/card = %q/%v, want the pinned card", ent.Slug, ent.Card)
	}
	if got := quota(t, ent); got != freeAllowance {
		t.Errorf("quota = %d, want %d", got, freeAllowance)
	}
	if ent.SubStatus != corebilling.SubStatusActive || ent.ProviderCustomerID != "cus_1" {
		t.Errorf("sub = %q/%q, want active/cus_1", ent.SubStatus, ent.ProviderCustomerID)
	}
}

// The operator's pin on the row wins over the mandate's: that is how an org is
// grandfathered by hand or moved onto new terms deliberately.
func TestRowPinOutranksTheMandatesPin(t *testing.T) {
	rec := corebilling.Record{Present: true, PlanSlug: corebilling.CurrentSlug}
	ent := corebilling.Resolve(created, rec, liveSub("usage-2020-01"), later, true)
	if ent.Slug != corebilling.CurrentSlug || ent.Card == nil {
		t.Errorf("slug = %q, want the row's pin over the mandate's unknown card", ent.Slug)
	}
}

// A mandate pinned to a card the catalog dropped keeps its name and no quota:
// resolving it to the current card would price it on terms nobody agreed to.
func TestMandateOnAnUnknownCardKeepsItsName(t *testing.T) {
	ent := corebilling.Resolve(created, corebilling.Record{}, liveSub("usage-2020-01"), later, true)
	if ent.Slug != "usage-2020-01" || ent.Card != nil {
		t.Errorf("slug/card = %q/%v, want the mandate's own slug and no card", ent.Slug, ent.Card)
	}
	if ent.Status != corebilling.StatusActive || !ent.Chargeable {
		t.Errorf("status/chargeable = %s/%v, want ACTIVE and chargeable", ent.Status, ent.Chargeable)
	}
	if ent.IncludedEvents != nil {
		t.Errorf("quota = %d, want absent", *ent.IncludedEvents)
	}
}

// A mandate opened for a deal: the terms are pug's, the money is the mandate's.
func TestCustomDealWithAMandate(t *testing.T) {
	ent := corebilling.Resolve(created, deal(40_000, 300, 5_000_000), liveSub(corebilling.SlugCustom), later, true)
	if ent.Slug != corebilling.SlugCustom || ent.Terms == nil {
		t.Fatalf("slug/terms = %q/%v, want custom", ent.Slug, ent.Terms)
	}
	if !ent.Chargeable || ent.Status != corebilling.StatusActive {
		t.Errorf("chargeable/status = %v/%s, want true/ACTIVE", ent.Chargeable, ent.Status)
	}
	if got := quota(t, ent); got != 5_000_000 {
		t.Errorf("quota = %d, want 5000000", got)
	}
}

// A lapsed deal under a live mandate falls to the card, not to free: the org is
// still a customer, and the card is what anyone without a deal pays.
func TestLapsedDealUnderAMandateFallsToTheCard(t *testing.T) {
	rec := deal(40_000, 300, 5_000_000)
	rec.ContractEndsAt = later.AddDate(0, 0, -1)
	ent := corebilling.Resolve(created, rec, liveSub(corebilling.SlugCustom), later, true)
	if ent.Slug != corebilling.CurrentSlug || ent.Card == nil || ent.Terms != nil {
		t.Errorf("slug = %q, want the current card", ent.Slug)
	}
	if !ent.Chargeable || ent.Status != corebilling.StatusActive {
		t.Errorf("chargeable/status = %v/%s, want true/ACTIVE — the mandate is still live", ent.Chargeable, ent.Status)
	}
	if got := quota(t, ent); got != freeAllowance {
		t.Errorf("quota = %d, want the card's %d", got, freeAllowance)
	}
}

// past_due is live on purpose: the card failed, the entitlement did not.
func TestPastDueMandateStaysLive(t *testing.T) {
	sub := liveSub(corebilling.CurrentSlug)
	sub.Status = corebilling.SubStatusPastDue
	ent := corebilling.Resolve(created, corebilling.Record{}, sub, later, true)
	if ent.Status != corebilling.StatusActive || !ent.Chargeable {
		t.Errorf("status/chargeable = %s/%v, want ACTIVE and chargeable", ent.Status, ent.Chargeable)
	}
}

// Every not-live status supplies nothing and the org falls to what is beneath.
func TestNotLiveMandateSuppliesNothing(t *testing.T) {
	for _, status := range corebilling.AllSubStatuses() {
		if status.Live() {
			continue
		}
		sub := liveSub("usage-2020-01")
		sub.Status = status
		ent := corebilling.Resolve(created, corebilling.Record{}, sub, later, true)
		if ent.Slug != corebilling.CurrentSlug || ent.Chargeable || ent.Status != corebilling.StatusFree {
			t.Errorf("%s mandate resolved %q/%s/chargeable=%v, want the current card, FREE, not chargeable",
				status, ent.Slug, ent.Status, ent.Chargeable)
		}
		if ent.SubStatus != "" {
			t.Errorf("%s mandate reported sub_status %q, want empty", status, ent.SubStatus)
		}
	}
}

// A recurring subscription that somehow got stored is live but not chargeable:
// charging it would bill the org twice.
func TestRecurringSubscriptionIsNotChargeable(t *testing.T) {
	sub := liveSub(corebilling.CurrentSlug)
	sub.OnDemand = false
	ent := corebilling.Resolve(created, corebilling.Record{}, sub, later, true)
	if ent.Chargeable {
		t.Error("a subscription that is not on-demand is chargeable")
	}
}

// Billing off is the self-hosted shape: no quota, and no mandate consulted.
func TestMandateIgnoredWhenBillingIsOff(t *testing.T) {
	ent := corebilling.Resolve(created, corebilling.Record{}, liveSub(corebilling.CurrentSlug), later, false)
	if ent.Slug != corebilling.SlugFree || ent.IncludedEvents != nil || ent.Chargeable {
		t.Errorf("slug = %q quota = %v chargeable = %v, want free with nothing", ent.Slug, ent.IncludedEvents, ent.Chargeable)
	}
	if ent.SubStatus != "" {
		t.Errorf("sub_status = %q, want empty", ent.SubStatus)
	}
}

// The quota window is the org's anniversary; the mandate's period is the
// provider's date. They are different questions and must not be conflated.
func TestMandatePeriodIsNotTheQuotaWindow(t *testing.T) {
	sub := liveSub(corebilling.CurrentSlug)
	sub.CurrentPeriodEnd = time.Date(2026, 6, 28, 9, 30, 0, 0, time.UTC)
	ent := corebilling.Resolve(created, corebilling.Record{}, sub, later, true)
	if ent.PeriodEnd.Equal(ent.SubPeriodEnd) {
		t.Fatal("quota window end equals the billing date; they are different questions")
	}
	if !ent.SubPeriodEnd.Equal(sub.CurrentPeriodEnd) {
		t.Errorf("sub_period_end = %s, want %s", ent.SubPeriodEnd, sub.CurrentPeriodEnd)
	}
}
