package domains

import (
	"errors"
	"strings"
	"testing"

	coreorgs "github.com/pug-sh/pug/internal/core/orgs"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/testutil"
	"github.com/rs/xid"
)

func TestMain(m *testing.M) { testutil.Main(m) }

func TestVerifyShowRelease(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	pg := testutil.SetupPostgres(t)
	t.Setenv("DATABASE_URL", pg.PgW.Config().ConnString())
	org, err := dbwrite.New(pg.PgW).CreateOrg(t.Context(), dbwrite.CreateOrgParams{ID: xid.New().String(), DisplayName: "acme"})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	cli, err := New(t.Context())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(cli.Close)

	var verify strings.Builder
	if err := cli.Verify(t.Context(), &verify, org.ID, "ACME.com."); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	for _, want := range []string{org.ID, `"acme"`, "verified by operator", "off", "allowed", "never"} {
		if !strings.Contains(verify.String(), want) {
			t.Errorf("Verify output is missing %q:\n%s", want, verify.String())
		}
	}

	var release strings.Builder
	if err := cli.Release(t.Context(), &release, org.ID, "acme.com"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if !strings.Contains(release.String(), "no org has added this domain") {
		t.Errorf("Release output = %q", release.String())
	}
	if err := cli.Release(t.Context(), &release, org.ID, "acme.com"); !errors.Is(err, coreorgs.ErrDomainNotFound) {
		t.Errorf("second release err = %v, want ErrDomainNotFound", err)
	}
}
