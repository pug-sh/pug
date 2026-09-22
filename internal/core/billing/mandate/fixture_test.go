package mandate_test

import (
	"testing"
	"time"

	"github.com/rs/xid"

	"github.com/pug-sh/pug/internal/core/billing/entitlement"
	"github.com/pug-sh/pug/internal/core/billing/mandate"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

const actor = "tester@localhost"

// The mandate tests drive both services: a checkout or a delivery is applied
// against an entitlement the test set up first.
type fixture struct {
	svc   *mandate.Service
	ent   *entitlement.Service
	pg    *testutil.TestPostgres
	orgID string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pg := testutil.SetupPostgres(t)

	org, err := dbwrite.New(pg.PgW).CreateOrg(t.Context(), dbwrite.CreateOrgParams{
		ID:          xid.New().String(),
		DisplayName: "acme",
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	testutil.SetOrgCreateTime(t, pg.PgW, org.ID, time.Date(2025, 3, 10, 0, 0, 0, 0, time.UTC))

	ent, err := entitlement.NewService(pg.PgRO, pg.PgW, true)
	if err != nil {
		t.Fatalf("new entitlement service: %v", err)
	}
	svc := mandate.NewService(pg.PgRO, pg.PgW, nil, ent)
	return &fixture{
		svc:   svc,
		ent:   ent,
		pg:    pg,
		orgID: org.ID,
	}
}
