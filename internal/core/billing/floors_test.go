package billing

import (
	"testing"

	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/testutil"
)

// The real catalog always has both floors, so the guard would otherwise never
// run. Swaps the catalog for its duration, hence living inside the package.
func TestNewServiceRefusesACatalogMissingAFloor(t *testing.T) {
	for _, missing := range []string{SlugFree, SlugTrial} {
		t.Run(missing, func(t *testing.T) {
			original := catalog
			t.Cleanup(func() { catalog = original })

			trimmed := make([]Plan, 0, len(original))
			for _, p := range original {
				if p.Slug != missing {
					trimmed = append(trimmed, p)
				}
			}
			catalog = trimmed

			// Construction never touches the pools, so nil reaches the check.
			if _, err := NewService(nil, nil, true, nil); err == nil {
				t.Fatalf("NewService accepted a catalog with no %q tier", missing)
			}
		})
	}
}

// The unbilled walk hardcodes the floor slugs, so a tier the catalog calls a floor
// but SQL does not would be reported as a paid plan nobody is charged for, on
// every reconcile pass. Reads isFloor, hence living inside the package.
func TestTheFloorSlugsAgreeBetweenGoAndSQL(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	ctx := t.Context()

	orgs := make(map[string]string, len(catalog))
	for _, p := range Plans() {
		// custom's check constraint demands a quota; no other tier needs one.
		var quota any
		if p.Slug == SlugCustom {
			quota = int64(1)
		}
		orgs[p.Slug] = seedOrg(t, pg)
		if _, err := pg.PgW.Exec(ctx,
			`insert into billing_entitlements (org_id, plan_slug, included_events_override)
			 values ($1, $2, $3)`, orgs[p.Slug], p.Slug, quota); err != nil {
			t.Fatalf("seed %s entitlement: %v", p.Slug, err)
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
	for _, p := range Plans() {
		if got, want := walked[orgs[p.Slug]], !p.isFloor(); got != want {
			t.Errorf("%s: walked as a paid plan with no subscription = %v, want %v", p.Slug, got, want)
		}
	}
}
