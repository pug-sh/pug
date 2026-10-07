package entitlement_test

import (
	"strings"
	"testing"
	"time"

	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"github.com/rs/xid"
)

// A row as 020 stored it, before usage billing: any tier, a quota on a deal, a
// product on any row with a quota, and a trial end. nil is NULL.
type legacyRow struct {
	plan     string
	events   any
	name     any
	product  any
	until    any
	trialEnd any
}

// seed023 stores row for a new org, against a database stepped down to 023.
func seed023(t *testing.T, pg *testutil.TestPostgres, row legacyRow) string {
	t.Helper()
	org, err := dbwrite.New(pg.PgW).CreateOrg(t.Context(), dbwrite.CreateOrgParams{
		ID:          xid.New().String(),
		DisplayName: "acme",
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if _, err := pg.PgW.Exec(t.Context(),
		`insert into billing_entitlements (org_id, plan_slug, included_events_override,
		   display_name_override, provider_product_id, contract_ends_at, trial_ends_at)
		 values ($1, $2, $3, $4, $5, $6, $7)`,
		org.ID, row.plan, row.events, row.name, row.product, row.until, row.trialEnd); err != nil {
		t.Fatalf("seed a 020 %s row: %v", row.plan, err)
	}
	return org.ID
}

// down023 steps the test's database back to the schema 024 migrates from.
func down023(t *testing.T, pg *testutil.TestPostgres) {
	t.Helper()
	if _, err := testutil.PostgresMigrations(t, pg).DownTo(t.Context(), 23); err != nil {
		t.Fatalf("migrate down to 023: %v", err)
	}
}

// 024 refuses rows its new checks would, and says which: Postgres names only the
// constraint, so an operator whose deploy stops here would otherwise have to find
// the row themselves. main's `pug billing set` writes every one of these.
func TestMigration024NamesEveryRowItCannotPlace(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	down023(t, pg)

	refused := map[string]string{
		"a deal with no product": seed023(t, pg, legacyRow{plan: "custom", events: int64(5_000_000)}),
		"a product on a tier": seed023(t, pg, legacyRow{
			plan: "growth", events: int64(1_000_000), product: "prod_g",
		}),
		// A deal wound down to a comp on main keeps its product.
		"a comp that kept a deal's product": seed023(t, pg, legacyRow{
			plan: "free", events: int64(1_000_000), product: "prod_f", until: time.Now().AddDate(0, 1, 0),
		}),
	}
	placeable := seed023(t, pg, legacyRow{plan: "growth", events: int64(1_000_000)})

	migrations := testutil.PostgresMigrations(t, pg)
	_, err := migrations.UpByOne(t.Context())
	if err == nil {
		t.Fatal("024 applied over rows its checks refuse")
	}
	for name, orgID := range refused {
		if !strings.Contains(err.Error(), orgID) {
			t.Errorf("%s: the error does not name org %s: %v", name, orgID, err)
		}
	}
	if strings.Contains(err.Error(), placeable) {
		t.Errorf("the error names org %s, whose row 024 can place: %v", placeable, err)
	}
	if version, err := migrations.GetDBVersion(t.Context()); err != nil || version != 23 {
		t.Errorf("version = %d (%v), want 23: a refused 024 must leave nothing applied", version, err)
	}
}

// Every row 024 changes is recorded, as every other write to the row is: a retired
// tier becomes free with its overrides, and an existing deal is pinned to the one
// plan there is. The history keeps what it recorded before, trial ends included.
func TestMigration024RecordsWhatItRewrites(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	ctx := t.Context()
	down023(t, pg)

	trialEnd := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	comp := seed023(t, pg, legacyRow{plan: "growth", events: int64(1_000_000), name: "Acme Growth"})
	trial := seed023(t, pg, legacyRow{plan: "trial", trialEnd: trialEnd})
	deal := seed023(t, pg, legacyRow{plan: "custom", events: int64(5_000_000), product: "prod_deal"})
	untouched := seed023(t, pg, legacyRow{plan: "free"})
	if _, err := pg.PgW.Exec(ctx,
		`insert into billing_entitlement_history (actor, id, org_id, plan_slug, trial_ends_at)
		 values ('ops', $1, $2, 'trial', $3)`, xid.New().String(), trial, trialEnd); err != nil {
		t.Fatalf("seed an extend-trial snapshot: %v", err)
	}

	migrations := testutil.PostgresMigrations(t, pg)
	if _, err := migrations.UpByOne(ctx); err != nil {
		t.Fatalf("024: %v", err)
	}

	want := map[string]struct {
		plan, basePlan string
		events         int64
		name           string
	}{
		comp:  {plan: "free", events: 1_000_000, name: "Acme Growth"},
		trial: {plan: "free"},
		deal:  {plan: "custom", basePlan: "usage-2026-10", events: 5_000_000},
	}
	for orgID, w := range want {
		var plan, basePlan, name string
		var events int64
		if err := pg.PgW.QueryRow(ctx,
			`select plan_slug, coalesce(base_plan_slug, ''), coalesce(included_events_override, 0),
			   coalesce(display_name_override, '')
			 from billing_entitlements where org_id = $1`, orgID).Scan(&plan, &basePlan, &events, &name); err != nil {
			t.Fatalf("read %s: %v", orgID, err)
		}
		if plan != w.plan || basePlan != w.basePlan || events != w.events || name != w.name {
			t.Errorf("row %s = %s/%q/%d/%q, want %s/%q/%d/%q",
				orgID, plan, basePlan, events, name, w.plan, w.basePlan, w.events, w.name)
		}

		var snapPlan, snapBase string
		var snapEvents int64
		if err := pg.PgW.QueryRow(ctx,
			`select plan_slug, coalesce(base_plan_slug, ''), coalesce(included_events_override, 0)
			 from billing_entitlement_history where org_id = $1 and actor = 'migration/024'`,
			orgID).Scan(&snapPlan, &snapBase, &snapEvents); err != nil {
			t.Errorf("%s: no single migration/024 snapshot: %v", orgID, err)
			continue
		}
		if snapPlan != w.plan || snapBase != w.basePlan || snapEvents != w.events {
			t.Errorf("%s: snapshot %s/%q/%d, want the row as 024 left it", orgID, snapPlan, snapBase, snapEvents)
		}
	}

	var snapshots int
	if err := pg.PgW.QueryRow(ctx,
		`select count(*) from billing_entitlement_history where org_id = $1`, untouched).Scan(&snapshots); err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	if snapshots != 0 {
		t.Errorf("024 recorded %d snapshots for a row it did not change", snapshots)
	}

	var kept time.Time
	if err := pg.PgW.QueryRow(ctx,
		`select trial_ends_at from billing_entitlement_history where org_id = $1 and actor = 'ops'`,
		trial).Scan(&kept); err != nil || !kept.Equal(trialEnd) {
		t.Errorf("the extend-trial snapshot's trial end = %v (%v), want %v kept", kept, err, trialEnd)
	}

	// Down reverses the schema over the rows Up left behind.
	if _, err := migrations.DownTo(ctx, 23); err != nil {
		t.Errorf("024 down: %v", err)
	}
}
