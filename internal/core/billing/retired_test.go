package billing

import (
	"errors"
	"testing"
	"time"

	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"github.com/rs/xid"
)

// A retired card cannot be granted to an org that does not already hold it, and
// its holder can still be re-set. The guard fires for the first time on the day
// of the first reprice, so it is wired here rather than only tested as a
// predicate.
func TestSetPlanRefusesARetiredCardExceptToItsHolder(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	original := catalog
	t.Cleanup(func() { catalog = original })
	next := copyCard(original[0])
	next.Slug = "usage-2027-01"
	old := copyCard(original[0])
	old.Retired = true
	catalog = []RateCard{old, next}

	pg := testutil.SetupPostgres(t)
	svc, err := NewService(pg.PgRO, pg.PgW, Config{Enabled: true}, nil)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	fresh := seedOrg(t, pg)
	if _, err := svc.SetPlan(t.Context(), fresh, "tester", Change{PlanSlug: old.Slug}); !errors.Is(err, ErrPlanRetired) {
		t.Errorf("granting a retired card = %v, want ErrPlanRetired", err)
	}
	if _, err := svc.SetPlan(t.Context(), fresh, "tester", Change{PlanSlug: next.Slug}); err != nil {
		t.Fatalf("granting the live card: %v", err)
	}

	holder := seedOrg(t, pg)
	if _, err := pg.PgW.Exec(t.Context(),
		`insert into billing_entitlements (org_id, plan_slug) values ($1, $2)`, holder, old.Slug); err != nil {
		t.Fatalf("seed the holder: %v", err)
	}
	rec, err := svc.SetPlan(t.Context(), holder, "tester", Change{PlanSlug: old.Slug})
	if err != nil {
		t.Fatalf("re-setting a holder's own retired card: %v", err)
	}
	if rec.PlanSlug != old.Slug {
		t.Errorf("holder = %q, want %q", rec.PlanSlug, old.Slug)
	}
}

func seedOrg(t *testing.T, pg *testutil.TestPostgres) string {
	t.Helper()
	org, err := dbwrite.New(pg.PgW).CreateOrg(t.Context(), dbwrite.CreateOrgParams{
		ID: xid.New().String(), DisplayName: "acme",
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	testutil.SetOrgCreateTime(t, pg.PgW, org.ID, time.Date(2025, 3, 10, 0, 0, 0, 0, time.UTC))
	return org.ID
}

// A malformed catalog fails wiring, not an invoice months later.
func TestNewServiceRefusesAMalformedCatalog(t *testing.T) {
	original := catalog
	t.Cleanup(func() { catalog = original })
	bad := copyCard(original[0])
	bad.Tiers = nil
	catalog = []RateCard{bad}

	if _, err := NewService(nil, nil, Config{}, nil); err == nil {
		t.Error("NewService accepted a card with no tiers")
	}
}
