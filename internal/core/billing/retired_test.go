package billing

import (
	"errors"
	"testing"
	"time"

	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"github.com/rs/xid"
)

// Repricing mints a new card and retires the old one (see the immutability rule
// in catalog.go). A holder keeps resolving against the card they agreed to; an
// org with no mandate agreed to nothing and sees the current one. No card is
// retired in the real catalog, so these swap it for their duration — which is why
// they live inside the package.
func TestRetiredCardKeepsResolvingForItsHolder(t *testing.T) {
	original := rateCards
	t.Cleanup(func() { rateCards = original })

	next := copyCard(original[0])
	next.Slug, next.FreeEvents = "usage-2027-01-1", 0
	old := copyCard(original[0])
	old.Retired = true
	rateCards = []RateCard{old, next}

	created := time.Date(2025, 1, 10, 0, 0, 0, 0, time.UTC)
	now := time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC)

	holder := &Subscription{PlanSlug: old.Slug, Status: SubStatusActive}
	pinned := Resolve(created, Record{}, holder, now, true)
	if pinned.Slug != old.Slug || pinned.Card == nil || pinned.Card.FreeEvents != old.FreeEvents {
		t.Errorf("a holder resolved %q, want the retired card %q with its own free allowance",
			pinned.Slug, old.Slug)
	}

	fresh := Resolve(created, Record{}, nil, now, true)
	if fresh.Slug != next.Slug || fresh.Card == nil || fresh.Card.FreeEvents != 0 {
		t.Errorf("an org with no mandate resolved %q, want the current card %q", fresh.Slug, next.Slug)
	}
	if got := CurrentCard().Slug; got != next.Slug {
		t.Errorf("CurrentCard = %q, want %q", got, next.Slug)
	}
}

// The write half: an operator must not put a new org on a card that has been
// withdrawn, or every reprice would go on handing out the superseded rates.
func TestRetiredCardCannotBeGrantedToANewOrg(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	original := rateCards
	t.Cleanup(func() { rateCards = original })
	retired := copyCard(original[0])
	retired.Slug, retired.Retired = "usage-2025-01-1", true
	rateCards = append([]RateCard{retired}, original...)

	pg := testutil.SetupPostgres(t)
	ctx := t.Context()
	svc, err := NewService(pg.PgRO, pg.PgW, true, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	newOrg := seedOrg(t, pg)
	if _, err := svc.SetPlan(ctx, newOrg, "tester", Change{PlanSlug: retired.Slug}); !errors.Is(err, ErrPlanRetired) {
		t.Fatalf("pinning a retired card on a new org: err = %v, want ErrPlanRetired", err)
	}
	// The live card is unaffected, so the guard refuses retirement rather than
	// everything.
	if _, err := svc.SetPlan(ctx, newOrg, "tester", Change{PlanSlug: CurrentCard().Slug}); err != nil {
		t.Errorf("pinning the current card: %v", err)
	}
}

// The other half of the rule, and the entire point of retiring rather than
// deleting a card: an org already on one keeps it and can still be renewed.
func TestRetiredCardIsStillRenewableByItsHolder(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	pg := testutil.SetupPostgres(t)
	ctx := t.Context()
	svc, err := NewService(pg.PgRO, pg.PgW, true, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	held := CurrentCard().Slug
	incumbent := seedOrg(t, pg)
	if _, err := svc.SetPlan(ctx, incumbent, "tester", Change{PlanSlug: held}); err != nil {
		t.Fatalf("seed the incumbent on the live card: %v", err)
	}

	original := rateCards
	t.Cleanup(func() { rateCards = original })
	retired := copyCard(original[0])
	retired.Retired = true
	next := copyCard(original[0])
	next.Slug = "usage-2027-01-1"
	rateCards = []RateCard{retired, next}

	// A renewal is a re-set with a new end date on unchanged terms.
	until := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := svc.SetPlan(ctx, incumbent, "tester", Change{
		PlanSlug:       held,
		ContractEndsAt: &until,
	}); err != nil {
		t.Errorf("renewing a holder of a now-retired card: %v", err)
	}

	newcomer := seedOrg(t, pg)
	if _, err := svc.SetPlan(ctx, newcomer, "tester", Change{PlanSlug: held}); !errors.Is(err, ErrPlanRetired) {
		t.Errorf("pinning the same card on a newcomer: err = %v, want ErrPlanRetired", err)
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
