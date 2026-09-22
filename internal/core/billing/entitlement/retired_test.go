package entitlement

import (
	"errors"
	"testing"
	"time"

	"github.com/pug-sh/pug/internal/core/billing"

	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"github.com/rs/xid"
)

// No tier is retired yet, so the guard has nothing in the real catalog to act on
// — and an unexercised guard is one that stops working without anyone noticing.
// This test appends a retired tier for its duration, which is why it lives inside
// the package rather than in entitlement_test.
//
// What it protects: repricing mints a new slug and retires the old one (see the
// immutability rule in plans.go). If the retired slug could still be granted,
// every reprice would go on handing out the superseded numbers.
func TestRetiredPlanCannotBeGrantedToANewOrg(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	original := rateCards
	t.Cleanup(func() { rateCards = original })
	rateCards = append(append([]RateCard(nil), original...), RateCard{
		Slug: "usage-2025-01-1", DisplayName: "Usage (2025)", Currency: billing.Currency,
		FreeEvents: 400_000, RetentionDays: CardRetentionDays, Retired: true,
		Tiers: []Tier{{UpToEvents: 0, CentsPerMillion: 5_000}},
	})

	pg := testutil.SetupPostgres(t)
	ctx := t.Context()
	svc, err := NewService(pg.PgRO, pg.PgW, true)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	newOrg := seedOrg(t, pg)
	if _, err := svc.SetPlan(ctx, newOrg, "tester", Change{PlanSlug: "usage-2025-01-1"}); !errors.Is(err, ErrPlanRetired) {
		t.Fatalf("granting a retired tier to a new org: err = %v, want ErrPlanRetired", err)
	}

	// A live tier is unaffected, so the guard is refusing retirement rather than
	// everything.
	if _, err := svc.SetPlan(ctx, newOrg, "tester", Change{PlanSlug: CurrentCard().Slug}); err != nil {
		t.Errorf("granting a live tier: %v", err)
	}

}

// Cards() must keep listing a retired tier: that list is what the webhook resolves
// an incoming product against, so dropping it rejects its holders' renewals AND
// cancellations permanently — one reprice freezes every incumbent's subscription.
func TestRetiredPlanStaysInThePlanList(t *testing.T) {
	original := rateCards
	t.Cleanup(func() { rateCards = original })
	rateCards = append(append([]RateCard(nil), original...), RateCard{
		Slug: "usage-2025-01-1", DisplayName: "Usage (2025)", Currency: billing.Currency,
		FreeEvents: 400_000, RetentionDays: CardRetentionDays, Retired: true,
		Tiers: []Tier{{UpToEvents: 0, CentsPerMillion: 5_000}},
	})

	var found bool
	for _, p := range Cards() {
		if p.Slug == "usage-2025-01-1" {
			found = true
		}
	}
	if !found {
		t.Error("a retired card is missing from Cards(), so nothing can map its product back to a slug")
	}
}

// The other half of the rule, and the entire point of retiring rather than
// deleting a tier: an org already on one keeps it and can still be renewed.
// Retiring an EXISTING slug needs no migration — the check constraint already
// knows it — so this seeds on the real `growth` and then retires it.
func TestRetiredPlanIsStillRenewableByItsHolder(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	ctx := t.Context()
	svc, err := NewService(pg.PgRO, pg.PgW, true)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	incumbent := seedOrg(t, pg)
	if _, err := svc.SetPlan(ctx, incumbent, "tester", Change{PlanSlug: CurrentCard().Slug}); err != nil {
		t.Fatalf("seed the incumbent on a live tier: %v", err)
	}

	held := CurrentCard().Slug
	original := rateCards
	t.Cleanup(func() { rateCards = original })
	// Retire the card the incumbent holds, and add a live successor so the catalog
	// still has exactly one current card -- which is what NewService requires.
	retired := append([]RateCard(nil), original...)
	for i := range retired {
		if retired[i].Slug == held {
			retired[i].Retired = true
		}
	}
	rateCards = append(retired, RateCard{
		Slug: "usage-2027-01-1", DisplayName: "Usage (2027)", Currency: billing.Currency,
		FreeEvents: 100_000, RetentionDays: CardRetentionDays,
		Tiers: []Tier{{UpToEvents: 0, CentsPerMillion: 6_000}},
	})

	// A renewal is a re-set with a new end date on unchanged terms.
	if _, err := svc.SetPlan(ctx, incumbent, "tester", Change{
		PlanSlug:       held,
		ContractEndsAt: new(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)),
	}); err != nil {
		t.Errorf("renewing a holder of a now-retired tier: %v", err)
	}

	newcomer := seedOrg(t, pg)
	if _, err := svc.SetPlan(ctx, newcomer, "tester", Change{PlanSlug: held}); !errors.Is(err, ErrPlanRetired) {
		t.Errorf("granting the same tier to a newcomer: err = %v, want ErrPlanRetired", err)
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
