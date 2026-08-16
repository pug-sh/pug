package billing

import (
	"errors"
	"testing"
	"time"

	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"github.com/rs/xid"
)

// No tier is retired yet, so the guard has nothing in the real catalog to act on
// — and an unexercised guard is one that stops working without anyone noticing.
// This test appends a retired tier for its duration, which is why it lives inside
// the package rather than in billing_test.
//
// What it protects: repricing mints a new slug and retires the old one (see the
// immutability rule in plans.go). If the retired slug could still be granted,
// every reprice would go on handing out the superseded numbers.
func TestRetiredPlanCannotBeGrantedToANewOrg(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	original := catalog
	t.Cleanup(func() { catalog = original })
	catalog = append(append([]Plan(nil), original...), Plan{
		Slug: "growth-v0", DisplayName: "Growth (2025)", Currency: "USD",
		PriceCents: i64(1_500), IncludedEvents: i64(400_000), Retired: true,
	})

	pg := testutil.SetupPostgres(t)
	ctx := t.Context()
	svc, err := NewService(pg.PgRO, pg.PgW, true)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	newOrg := seedOrg(t, pg)
	if _, err := svc.SetPlan(ctx, newOrg, "tester", Change{PlanSlug: "growth-v0"}); !errors.Is(err, ErrPlanRetired) {
		t.Fatalf("granting a retired tier to a new org: err = %v, want ErrPlanRetired", err)
	}

	// A live tier is unaffected, so the guard is refusing retirement rather than
	// everything.
	if _, err := svc.SetPlan(ctx, newOrg, "tester", Change{PlanSlug: "growth"}); err != nil {
		t.Errorf("granting a live tier: %v", err)
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
	if _, err := svc.SetPlan(ctx, incumbent, "tester", Change{PlanSlug: "growth"}); err != nil {
		t.Fatalf("seed the incumbent on a live tier: %v", err)
	}

	original := catalog
	t.Cleanup(func() { catalog = original })
	retired := append([]Plan(nil), original...)
	for i := range retired {
		if retired[i].Slug == "growth" {
			retired[i].Retired = true
		}
	}
	catalog = retired

	// A renewal is a re-set with a new end date on unchanged terms.
	if _, err := svc.SetPlan(ctx, incumbent, "tester", Change{
		PlanSlug:       "growth",
		ContractEndsAt: new(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)),
	}); err != nil {
		t.Errorf("renewing a holder of a now-retired tier: %v", err)
	}

	newcomer := seedOrg(t, pg)
	if _, err := svc.SetPlan(ctx, newcomer, "tester", Change{PlanSlug: "growth"}); !errors.Is(err, ErrPlanRetired) {
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
