package mandate_test

import (
	"testing"

	"github.com/pug-sh/pug/internal/core/billing/entitlement"
	"github.com/pug-sh/pug/internal/core/billing/mandate"
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

// newFixture is billing on with no provider; newPaidFixture adds one.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	pg := testutil.SetupPostgres(t)

	orgID, err := dbwriteOrg(t, pg)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	ent, err := entitlement.NewService(pg.PgRO, pg.PgW, true)
	if err != nil {
		t.Fatalf("new entitlement service: %v", err)
	}
	return &fixture{
		svc:   mandate.NewService(pg.PgRO, pg.PgW, nil, ent),
		ent:   ent,
		pg:    pg,
		orgID: orgID,
	}
}
