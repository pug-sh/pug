package billing_test

import (
	"testing"
	"time"

	corebilling "github.com/pug-sh/pug/internal/core/billing"
)

func liveSub(slug string) *corebilling.Subscription {
	return &corebilling.Subscription{
		PlanSlug: slug, Status: corebilling.SubStatusActive,
		Currency:           "USD",
		ProviderCustomerID: "cus_1", ProviderSubID: "sub_1",
		CurrentPeriodEnd: later.AddDate(0, 0, 12),
	}
}

// A mandate is what makes an org chargeable; nothing else does.
func TestMandateMakesTheOrgChargeable(t *testing.T) {
	card := currentCard()

	ent := corebilling.Resolve(created, corebilling.Record{}, liveSub(card.Slug), later, true)
	if !ent.Chargeable {
		t.Error("an org with a live mandate is not chargeable")
	}
	if ent.Status != corebilling.StatusActive {
		t.Errorf("status = %s, want ACTIVE", ent.Status)
	}
	if ent.SubStatus != corebilling.SubStatusActive {
		t.Errorf("sub_status = %q, want active", ent.SubStatus)
	}
	if ent.ProviderCustomerID != "cus_1" {
		t.Errorf("provider_customer_id = %q, want cus_1", ent.ProviderCustomerID)
	}

	if none := corebilling.Resolve(created, corebilling.Record{}, nil, later, true); none.Chargeable {
		t.Error("an org with no mandate is chargeable")
	}
}

// The card the checkout pinned is carried on the mandate, and that is what the
// org is priced on — a reprice does not move a customer who already agreed.
func TestMandatePinsTheCard(t *testing.T) {
	ent := corebilling.Resolve(created, corebilling.Record{}, liveSub("usage-2019-01-1"), later, true)
	if ent.Slug != "usage-2019-01-1" {
		t.Errorf("slug = %q, want the card the mandate pinned", ent.Slug)
	}
}

// The operator's pin outranks the mandate's: it is how a customer is
// grandfathered by hand or moved onto new terms deliberately.
func TestOperatorPinOutranksTheMandatesCard(t *testing.T) {
	card := currentCard()
	rec := corebilling.Record{Present: true, PlanSlug: card.Slug}
	ent := corebilling.Resolve(created, rec, liveSub("usage-2019-01-1"), later, true)
	if ent.Slug != card.Slug {
		t.Errorf("slug = %q, want the operator's pin %q", ent.Slug, card.Slug)
	}
}

// past_due is live on purpose: the card failed, the entitlement did not.
func TestPastDueStaysChargeable(t *testing.T) {
	card := currentCard()
	sub := liveSub(card.Slug)
	sub.Status = corebilling.SubStatusPastDue

	ent := corebilling.Resolve(created, corebilling.Record{}, sub, later, true)
	if !ent.Chargeable {
		t.Error("a past_due mandate is not chargeable; a failed card is still a card")
	}
	if ent.Slug != card.Slug {
		t.Errorf("slug = %q, want %q — a failed card must not degrade the plan", ent.Slug, card.Slug)
	}
}

// Every not-live status supplies nothing: the org keeps the current card and
// stops being chargeable.
func TestNotLiveSubscriptionSuppliesNothing(t *testing.T) {
	card := currentCard()
	for _, status := range corebilling.AllSubStatuses() {
		if status.Live() {
			continue
		}
		sub := liveSub("usage-2019-01-1")
		sub.Status = status
		ent := corebilling.Resolve(created, corebilling.Record{}, sub, later, true)
		if ent.Slug != card.Slug {
			t.Errorf("%s subscription resolved to %q, want the current card", status, ent.Slug)
		}
		if ent.SubStatus != "" {
			t.Errorf("%s subscription reported sub_status %q, want empty", status, ent.SubStatus)
		}
		if ent.Chargeable {
			t.Errorf("%s subscription left the org chargeable", status)
		}
	}
}

// A deal is priced on its own terms whether or not a mandate is behind it; the
// mandate decides only whether the invoice can be collected.
func TestDealIsPricedWithOrWithoutAMandate(t *testing.T) {
	rec := deal()

	withMandate := corebilling.Resolve(created, rec, liveSub(currentCard().Slug), later, true)
	if withMandate.Terms == nil || !withMandate.Chargeable {
		t.Errorf("a deal with a mandate: terms=%v chargeable=%v, want both", withMandate.Terms, withMandate.Chargeable)
	}
	withoutMandate := corebilling.Resolve(created, rec, nil, later, true)
	if withoutMandate.Terms == nil {
		t.Error("a deal with no mandate lost its terms; it is still what the org agreed to")
	}
	if withoutMandate.Chargeable {
		t.Error("a deal with no mandate is chargeable; there is nothing to charge")
	}
}

// A lapsed deal falls to the current card even with a live mandate: the deal
// ended, the customer is still a customer, and the card is what they now pay.
func TestLapsedDealWithALiveMandateFallsToTheCurrentCard(t *testing.T) {
	card := currentCard()
	rec := deal()
	rec.DisplayNameOverride = "Acme Pilot"
	rec.ContractEndsAt = later.AddDate(0, 0, -1)

	ent := corebilling.Resolve(created, rec, liveSub(card.Slug), later, true)
	if ent.Terms != nil {
		t.Errorf("terms = %+v after the deal lapsed, want none", *ent.Terms)
	}
	if ent.Slug != card.Slug || ent.Card == nil {
		t.Errorf("slug = %q, want the current card %q", ent.Slug, card.Slug)
	}
	if got := quota(t, ent); got != card.FreeEvents {
		t.Errorf("allowance = %d, want the card's %d", got, card.FreeEvents)
	}
	if ent.DisplayName != card.DisplayName {
		t.Errorf("display name = %q, want the card's — the pilot's name lapsed with it", ent.DisplayName)
	}
	// Still chargeable: the mandate outlives the deal.
	if !ent.Chargeable {
		t.Error("a live mandate stopped being chargeable when the deal lapsed")
	}
}

// Billing off is the self-hosted shape: no mandate consulted and nothing priced.
func TestSubscriptionIgnoredWhenBillingIsOff(t *testing.T) {
	ent := corebilling.Resolve(created, corebilling.Record{}, liveSub(currentCard().Slug), later, false)
	if ent.Slug != corebilling.SlugFree || ent.Card != nil {
		t.Errorf("slug = %q card = %v, want free with nothing priced", ent.Slug, ent.Card)
	}
	if ent.SubStatus != "" {
		t.Errorf("sub_status = %q, want empty", ent.SubStatus)
	}
	if ent.Chargeable {
		t.Error("chargeable with billing off")
	}
}

// The quota window is the org's anniversary; the subscription's period is when
// the provider bills. They are different questions and must not be conflated.
func TestSubscriptionPeriodIsNotTheQuotaWindow(t *testing.T) {
	sub := liveSub(currentCard().Slug)
	sub.CurrentPeriodEnd = time.Date(2026, 6, 28, 9, 30, 0, 0, time.UTC)
	ent := corebilling.Resolve(created, corebilling.Record{}, sub, later, true)
	if ent.PeriodEnd.Equal(ent.SubPeriodEnd) {
		t.Fatal("quota window end equals the billing date; they are different questions")
	}
	if !ent.SubPeriodEnd.Equal(sub.CurrentPeriodEnd) {
		t.Errorf("sub_period_end = %s, want %s", ent.SubPeriodEnd, sub.CurrentPeriodEnd)
	}
}
