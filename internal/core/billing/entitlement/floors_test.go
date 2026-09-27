package entitlement

import (
	"strings"
	"testing"

	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/testutil"
)

func TestValidateCatalog(t *testing.T) {
	good := Plan{Slug: "usage-x", DisplayName: "X", FreeEvents: 10, TierUpTo: []int64{20, 30}, RetentionDays: 365}
	cases := []struct {
		name    string
		plans   []Plan
		wantErr string
	}{
		{"the real catalog", catalog, ""},
		{"a plan on sale", []Plan{good}, ""},
		{"nothing on sale", []Plan{withRetired(good)}, "no plan on sale"},
		{"a plan named free", []Plan{withSlug(good, SlugFree)}, "reserved slug"},
		{"a plan named custom", []Plan{withSlug(good, SlugCustom)}, "reserved slug"},
		{"a duplicate", []Plan{good, good}, "twice"},
		{"a bound at the allowance", []Plan{withTiers(good, []int64{10, 30})}, "rise above"},
		{"bounds out of order", []Plan{withTiers(good, []int64{30, 20})}, "rise above"},
		{"no retention", []Plan{{Slug: "usage-y", FreeEvents: 1, TierUpTo: []int64{2}}}, "no retention"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCatalog(tc.plans)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("validateCatalog: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

// NewService checks the catalog at wiring time, not on the first request. Swaps the
// catalog for its duration, hence living inside the package.
func TestNewServiceRefusesACatalogWithNothingOnSale(t *testing.T) {
	original := catalog
	t.Cleanup(func() { catalog = original })
	catalog = []Plan{withRetired(original[0])}
	// Construction never touches the pools, so nil reaches the check.
	if _, err := NewService(nil, nil, true); err == nil {
		t.Fatal("NewService accepted a catalog with no plan on sale")
	}
}

// The reconcile pass walks deals with no live subscription: staged and not yet
// bought, or lapsed while their contract runs. A free row is ordinary, and a row
// naming a usage plan is not a deal. Inside the package for seedOrg.
func TestTheWalkSelectsOnlyDeals(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	ctx := t.Context()

	rows := map[string]struct {
		slug    string
		product any
	}{
		"free":  {SlugFree, nil},
		"deal":  {SlugCustom, "prod_deal"},
		"usage": {SlugUsage, nil},
	}
	orgs := make(map[string]string, len(rows))
	for name, row := range rows {
		orgs[name] = seedOrg(t, pg)
		if _, err := pg.PgW.Exec(ctx,
			`insert into billing_entitlements (org_id, plan_slug, provider_product_id) values ($1, $2, $3)`,
			orgs[name], row.slug, row.product); err != nil {
			t.Fatalf("seed %s entitlement: %v", name, err)
		}
	}

	walkedRows, err := dbread.New(pg.PgW).ListPaidEntitlementsWithoutLiveSubscription(ctx)
	if err != nil {
		t.Fatalf("ListPaidEntitlementsWithoutLiveSubscription: %v", err)
	}
	walked := make(map[string]bool, len(walkedRows))
	for _, row := range walkedRows {
		walked[row.OrgID] = true
	}
	for name := range rows {
		if got, want := walked[orgs[name]], name == "deal"; got != want {
			t.Errorf("%s: walked as a deal with no subscription = %v, want %v", name, got, want)
		}
	}
}

func withRetired(p Plan) Plan              { p.Retired = true; return p }
func withSlug(p Plan, slug string) Plan    { p.Slug = slug; return p }
func withTiers(p Plan, tiers []int64) Plan { p.TierUpTo = tiers; return p }
