package entitlement

import (
	"testing"

	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/testutil"
)

// The unbilled walk hardcodes the slugs that are not a grant, so a slug Go calls
// a pin but SQL does not would be reported as a paid org nobody charges on every
// reconcile pass, and the reverse would hide one. Every state and every card is
// seeded, so a new state, or a card slugged like one, fails here. Inside the
// package for pinned and seedOrg.
func TestTheUnpinnedSlugsAgreeBetweenGoAndSQL(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	ctx := t.Context()

	slugs := []string{SlugFree, SlugTrial, SlugCustom}
	for _, c := range Cards() {
		slugs = append(slugs, c.Slug)
	}
	orgs := make(map[string]string, len(slugs))
	for _, slug := range slugs {
		// custom's check constraint demands an allowance; no other slug needs one.
		var allowance any
		if slug == SlugCustom {
			allowance = int64(1)
		}
		orgs[slug] = seedOrg(t, pg)
		if _, err := pg.PgW.Exec(ctx,
			`insert into billing_entitlements (org_id, plan_slug, included_events_override)
			 values ($1, $2, $3)`, orgs[slug], slug, allowance); err != nil {
			t.Fatalf("seed %s entitlement: %v", slug, err)
		}
	}

	rows, err := dbread.New(pg.PgW).ListPaidEntitlementsWithoutLiveSubscription(ctx)
	if err != nil {
		t.Fatalf("ListPaidEntitlementsWithoutLiveSubscription: %v", err)
	}
	walked := make(map[string]bool, len(rows))
	for _, row := range rows {
		walked[row.OrgID] = true
	}
	for _, slug := range slugs {
		if got, want := walked[orgs[slug]], pinned(slug); got != want {
			t.Errorf("%s: walked as a grant with no subscription = %v, want %v", slug, got, want)
		}
	}
}
