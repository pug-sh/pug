package retention

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rs/xid"

	coreretention "github.com/pug-sh/pug/internal/core/retention"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
)

func TestMain(m *testing.M) { testutil.Main(m) }

func TestExpire(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	t.Setenv("DATABASE_URL", pg.PgW.Config().ConnString())
	ctx := t.Context()

	orgID := xid.New().String()
	if _, err := dbwrite.New(pg.PgW).CreateOrg(ctx, dbwrite.CreateOrgParams{ID: orgID, DisplayName: "Org"}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := pg.PgW.Exec(ctx,
		"insert into retention_state (days, org_id, pending_days) values (1825, $1, 365)", orgID); err != nil {
		t.Fatalf("seed retention state: %v", err)
	}

	cli, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(cli.Close)

	var out strings.Builder
	if err := cli.Expire(ctx, &out, orgID, " "); !errors.Is(err, coreretention.ErrActorRequired) {
		t.Fatalf("Expire with a blank actor = %v, want ErrActorRequired", err)
	}
	if err := cli.Expire(ctx, &out, orgID, "ops/1"); err != nil {
		t.Fatalf("Expire: %v", err)
	}
	until := time.Now().UTC().Add(30 * 24 * time.Hour).Format(time.DateOnly)
	if want := orgID + ": keeps 1825 days until " + until + ", then 365 days"; !strings.HasPrefix(out.String(), want) {
		t.Errorf("Expire = %q, want it to start %q", out.String(), want)
	}
	var by string
	if err := pg.PgW.QueryRow(ctx,
		"select expired_by from retention_state where org_id = $1 and pending_since is not null", orgID).Scan(&by); err != nil || by != "ops/1" {
		t.Errorf("expired_by = %q, %v; want ops/1 with the wait started", by, err)
	}
	if err := cli.Expire(ctx, &out, orgID, "ops/2"); !errors.Is(err, coreretention.ErrNothingWaiting) {
		t.Errorf("a second Expire = %v, want ErrNothingWaiting", err)
	}
}
